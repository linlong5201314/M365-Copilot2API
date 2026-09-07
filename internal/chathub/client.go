package chathub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const (
	rs          = "\x1e"
	defaultTone = "magic"
	wsBase      = "wss://substrate.office.com/m365Copilot/Chathub"
	// maxAttachments bounds per-request remote downloads: each image is
	// base64-encoded and held in memory alongside the multipart body.
	maxAttachments   = 10
	maxAttachmentMiB = 10
)

// Variants mirrored from the verified browser / Python probe.
const variants = "EnableMcpServerWidgets,feature.EnableMcpServerWidgets,feature.EnableLuForChatCIQ,feature.enableChatCIQPlugin,EnableRequestPlugins,feature.EnableSensitivityLabels,EnableUnsupportedUrlDetector,feature.IsCustomEngineCopilotEnabled,feature.bizchatfluxv3,feature.enablechatpages,feature.enableCodeCanvas,feature.turnOnWorkTabRecommendation,turnOffWorkTabUpsellFromClient,feature.turnOnDARecommendation,feature.IsStreamingModeInChatRequestEnabled,IncludeSourceAttributionsConcise,SkipPublishEmptyMessage,feature.EnableDeduplicatingSourceAttributions,Enable3PActionProgressMessages,feature.enableClientWebRtc,feature.EnableMeetingRecapOfSeriesMeetingWithCiq,feature.EnableReferencesListCompleteSignal,feature.StorageMessageSplitDisabled,feature.EnableCuaTakeControlApi,feature.cwcallowedos,feature.disabledisallowedmsgs,feature.enableCitationsForSynthesisData,feature.enableGenerateGraphicArtOptionsSet,cdximagen,feature.EnableUpdatedUXForConfirmationDialog,feature.EnableClientFileURLSupportForOfficeWebPaidCopilot,feature.EnableDesignEditorImageGrounding,feature.EnableDesignerEditor,feature.OfficeWebToHelix,feature.OfficeDesktopToHelix,feature.M365TeamsHubToHelix,feature.OwaHubToHelix,feature.MonarchHubToHelix,feature.Win32OutlookHubToHelix,feature.MacOutlookHubToHelix,Agt_bizchat_enableGpt5ForHelix"

type Account struct {
	AccessToken string
	OID         string
	TID         string
}

type Request struct {
	Text           string
	Tone           string
	ConversationID string
	SessionID      string
	Attachments    []Attachment
	Tools          []Tool
	ToolChoice     any
	MCPServerURL   string // URL of the MCP HTTP SSE server for tool discovery
	// AgentGptID attaches a published Copilot Studio declarative agent via
	// threadLevelGptId.gpts. Upstream forces the tone to GPT-5 while an
	// agent is attached, so callers set it only on tool-bearing answer turns.
	AgentGptID string
	// Started is true only for the first turn of a ChatHub conversation.
	Started bool
}

// StreamEvent is the protocol-neutral event exposed while ChatHub is still
// producing a response. Text events are safe to show immediately; progress and
// tool events are normally buffered by protocol adapters.
type StreamEvent struct {
	Kind        string
	Text        string
	MessageType string
	ContentType string
	ToolName    string
	Arguments   json.RawMessage
	Raw         json.RawMessage
}

type StreamHandler func(StreamEvent) error

// ThrottleError marks an upstream throttling outcome that ChatHub delivers
// out-of-band (type 3 error frame or result.value) instead of as an HTTP
// status. The web layer maps it to rate_limit through errors.As instead of
// string-matching free-form error text.
type ThrottleError struct {
	Value string
}

func (e *ThrottleError) Error() string {
	return "upstream throttled: " + e.Value
}

// throttleSignal reports whether a ChatHub error message or result value
// indicates the account is being rate limited or throttled.
func throttleSignal(v string) bool {
	if v == "" {
		return false
	}
	lv := strings.ToLower(v)
	return strings.Contains(lv, "throttl") ||
		strings.Contains(lv, "too many requests") ||
		strings.Contains(lv, "429")
}

// DisengagedError marks ChatHub's Disengaged gate: upstream suppressed the
// turn and produced no usable output. Retrying immediately usually makes it
// worse; the web layer applies a longer cooldown than for rate limits.
type DisengagedError struct {
	Message string
}

func (e *DisengagedError) Error() string {
	return "upstream disengaged: " + e.Message
}

type Result struct {
	Text           string
	Reasoning      string
	ConversationID string
	SessionID      string
	RequestID      string
	Throttling     any
	RawResult      string
	// ContentOrigin is the last contentOrigin seen on bot text (e.g. DeepLeo
	// for real answers, BotConnection for canned filler). The model tester
	// uses it to tell a live tone from a registered-but-dead one.
	ContentOrigin string
	Events        []json.RawMessage
	Normalized    []Event
	Images        []string
}

type Client struct {
	HTTPHeader http.Header
	HTTPClient *http.Client
	Dialer     *websocket.Dialer
	// WSBase overrides the ChatHub WebSocket origin; tests point this at a
	// fake SignalR server.
	WSBase string
	// Trace receives attachment-only metadata; URL contents are never exposed.
	Trace func(map[string]any)
}

func NewClient() *Client {
	h := make(http.Header)
	h.Set("Origin", "https://m365.cloud.microsoft")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0")
	return &Client{
		HTTPHeader: h,
		HTTPClient: outbound.HTTPClient(),
		Dialer:     outbound.WebSocketDialer(),
	}
}

func (c *Client) Chat(ctx context.Context, acc Account, req Request) (Result, error) {
	return c.ChatWithDelta(ctx, acc, req, nil)
}

// ChatWithEvents is the compatibility entry point for the full event stream.
// The initial implementation exposes every upstream text delta immediately;
// the existing ChatWithDelta path remains the source of truth until the
// SignalR frame parser is migrated to emit progress/tool events as well.
func (c *Client) ChatWithEvents(ctx context.Context, acc Account, req Request, handler StreamHandler) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, func(text string) error {
		if handler == nil {
			return nil
		}
		return handler(StreamEvent{Kind: "text", Text: text})
	}, handler)
}

// ChatWithDelta preserves Chat semantics while exposing upstream text deltas as
// soon as SignalR delivers them. onDelta must return quickly; returning an error
// cancels the request. Full snapshot messages are retained for final-result
// reconstruction but are not emitted as deltas, preventing duplicate text.
func (c *Client) ChatWithDelta(ctx context.Context, acc Account, req Request, onDelta func(string) error) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, onDelta, nil)
}

func (c *Client) chatWithHandlers(ctx context.Context, acc Account, req Request, onDelta func(string) error, onEvent StreamHandler) (Result, error) {
	return c.chatWithHandlersConn(ctx, acc, req, onDelta, onEvent, nil)
}

// ChatPreconn runs a complete chat invocation on a connection returned by
// Preconnect, skipping the dial and handshake entirely. acc is only a
// fallback when pre is nil.
func (c *Client) ChatPreconn(ctx context.Context, acc Account, pre *Preconn, req Request) (Result, error) {
	if pre != nil {
		acc = pre.acc
	}
	return c.chatWithHandlersConn(ctx, acc, req, nil, nil, pre)
}

// ChatWithEventsPreconn is ChatWithEvents on a pre-connected socket.
func (c *Client) ChatWithEventsPreconn(ctx context.Context, acc Account, pre *Preconn, req Request, handler StreamHandler) (Result, error) {
	if pre != nil {
		acc = pre.acc
	}
	return c.chatWithHandlersConn(ctx, acc, req, func(text string) error {
		if handler == nil {
			return nil
		}
		return handler(StreamEvent{Kind: "text", Text: text})
	}, handler, pre)
}

func (c *Client) chatWithHandlersConn(ctx context.Context, acc Account, req Request, onDelta func(string) error, onEvent StreamHandler, pre *Preconn) (Result, error) {
	startedAt := time.Now()
	log.Printf("chathub timing start prompt_len=%d", len(req.Text))
	if acc.AccessToken == "" || acc.OID == "" || acc.TID == "" {
		return Result{}, fmt.Errorf("missing access token / oid / tid")
	}
	if strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0 {
		return Result{}, fmt.Errorf("empty prompt and no attachments")
	}
	if req.Tone == "" {
		req.Tone = defaultTone
	}
	firstTurn := req.Started
	var conn *websocket.Conn
	var requestID string
	if pre != nil {
		// A pre-connected socket already carries the conversation identifiers
		// in its URL; the chat frame must echo exactly those.
		conn = pre.conn
		req.ConversationID = pre.conversationID
		req.SessionID = pre.sessionID
		requestID = pre.requestID
	} else {
		if req.SessionID == "" {
			req.SessionID = uuid.NewString()
			firstTurn = true
		}
		if req.ConversationID == "" {
			req.ConversationID = uuid.NewString()
			firstTurn = true
		}
		requestID = uuid.NewString()
		if err := c.uploadAttachments(ctx, acc, req.ConversationID, req.Attachments); err != nil {
			return Result{}, fmt.Errorf("upload attachment: %w", err)
		}

		wsURL, err := c.buildWSURL(acc, req.SessionID, req.ConversationID, requestID)
		if err != nil {
			return Result{}, err
		}

		dialStarted := time.Now()
		var dialErr error
		conn, _, dialErr = c.Dialer.DialContext(ctx, wsURL, c.HTTPHeader.Clone())
		log.Printf("chathub timing ws_dial_ms=%d total_ms=%d", time.Since(dialStarted).Milliseconds(), time.Since(startedAt).Milliseconds())
		if dialErr != nil {
			return Result{}, fmt.Errorf("ws dial: %w", dialErr)
		}
		if err := signalRHandshake(conn); err != nil {
			_ = conn.Close()
			return Result{}, err
		}
	}
	defer conn.Close()

	// stopGeneration asks ChatHub to abort the in-flight invocation (same
	// invocationId "0" as the chat frame). Sent when the caller gives up —
	// client disconnect, request deadline, or handler error — so the upstream
	// turn stops consuming the account's per-conversation message quota.
	stopGeneration := func() {
		_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":1,"target":"stop","invocationId":"0"}`+rs))
	}
	// abort funnels every give-up path through the stop frame.
	abort := func(err error) (Result, error) {
		stopGeneration()
		return Result{}, err
	}

	payload := chatPayload(req.Text, req.SessionID, req.ConversationID, requestID, req.Tone, firstTurn, req.Attachments, req.Tools, req.ToolChoice, req.MCPServerURL, req.AgentGptID)
	log.Printf("chathub prompt-trace text=%d tools=%d payload=%d", len(req.Text), len(req.Tools), len(payload))
	if c.Trace != nil {
		meta := map[string]any{"stage": "chathub_payload", "attachment_count": len(req.Attachments), "payload_has_attachments": strings.Contains(payload, `"attachments"`), "attachments": []map[string]any{}}
		for _, a := range req.Attachments {
			meta["attachments"] = append(meta["attachments"].([]map[string]any), map[string]any{"type": a.Type, "mime_type": a.MimeType, "url_length": len(a.URL), "data_url": strings.HasPrefix(a.URL, "data:"), "name": a.Name})
		}
		c.Trace(meta)
	}
	log.Printf("chathub timing handshake_ms=%d", time.Since(startedAt).Milliseconds())
	payloadSentAt := time.Now()
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		return Result{}, fmt.Errorf("chat send: %w", err)
	}

	var deltas []string
	var streamed strings.Builder
	emitDelta := func(d string) error {
		if d == "" {
			return nil
		}
		if streamed.Len() == 0 {
			log.Printf("chathub timing first_delta_ms=%d len=%d", time.Since(payloadSentAt).Milliseconds(), len(d))
		}
		streamed.WriteString(d)
		deltas = append(deltas, d)
		if onDelta != nil {
			return onDelta(d)
		}
		return nil
	}
	// ChatHub signals text either as a full snapshot or as cursor rewrites.
	// Only the portion not already streamed may be emitted; naive prefix
	// checks misfire when upstream rewrites the whole buffer, which duplicated
	// answers (AAA…). Match any overlap and emit the tail.
	emitSnapshot := func(snapshot string) error {
		if snapshot == "" {
			return nil
		}
		cur := streamed.String()
		if cur == "" {
			return emitDelta(snapshot)
		}
		if strings.HasPrefix(snapshot, cur) {
			return emitDelta(strings.TrimPrefix(snapshot, cur))
		}
		if i := strings.Index(snapshot, cur); i >= 0 {
			return emitDelta(snapshot[i+len(cur):])
		}
		if len(snapshot) > len(cur) && strings.HasSuffix(snapshot, cur) {
			return emitDelta(snapshot[:len(snapshot)-len(cur)])
		}
		return emitDelta(snapshot)
	}
	var final string
	var throttling any
	var rawResult string
	var events []json.RawMessage
	seenStreamTools := map[string]bool{}
	var reasoningBuf strings.Builder
	disengagedSeen := false
	var lastContentOrigin string

	deadline := time.Now().Add(5 * time.Minute)
	type wsRead struct {
		msg []byte
		err error
	}
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		// ReadMessage 阻塞期间无法响应 ctx 取消，放入独立 goroutine 由 select 联动。
		readCh := make(chan wsRead, 1)
		go func() {
			_, msg, err := conn.ReadMessage()
			readCh <- wsRead{msg: msg, err: err}
		}()
		var read wsRead
		select {
		case <-ctx.Done():
			return abort(ctx.Err())
		case read = <-readCh:
		}
		if read.err != nil {
			// Never convert a timeout or dropped WebSocket into a successful
			// partial response. A response is complete only after SignalR type 3.
			return Result{}, fmt.Errorf("ws read before completion: %w", read.err)
		}
		for _, part := range strings.Split(string(read.msg), rs) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			events = append(events, json.RawMessage(append([]byte(nil), part...)))
			var obj map[string]any
			if err := json.Unmarshal([]byte(part), &obj); err != nil {
				continue
			}
			t, _ := obj["type"].(float64)
			target, _ := obj["target"].(string)

			// SignalR ping
			if int(t) == 6 {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+rs))
				continue
			}

			if int(t) == 1 && target == "update" {
				args, _ := obj["arguments"].([]any)
				for _, raw := range args {
					arg, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					msgs, _ := arg["messages"].([]any)
					if onEvent != nil {
						for _, ev := range extractToolEvents(arg, seenStreamTools) {
							if err := onEvent(ev); err != nil {
								return abort(err)
							}
						}
					}

					for _, ev := range classifyUpdateMessages(msgs) {
						if ev.Kind == "reasoning" {
							reasoningBuf.WriteString(ev.Text)
						}
						if ev.Kind == "disengaged" {
							disengagedSeen = true
						}
						ev.Raw = eventRaw(arg)
						// SearchResults frames carry live citations; surface them to
						// handlers even though they are classified as text.
						if (ev.Kind != "text" || ev.ContentType == "SearchResults") && onEvent != nil {
							if err := onEvent(ev); err != nil {
								return abort(err)
							}
						}
					}
					toolFrame := false
					for _, mraw := range msgs {
						m, _ := mraw.(map[string]any)
						mt, _ := m["messageType"].(string)
						ct, _ := m["contentType"].(string)
						if mt == "Progress" || ct == "SearchResults" || ct == "Code" || ct == "ToolCall" {
							toolFrame = true
						}
					}
					if w, ok := arg["writeAtCursor"].(string); ok && w != "" && !toolFrame {
						if err := emitSnapshot(w); err != nil {
							return abort(err)
						}
					}
					if thr, ok := arg["throttling"]; ok {
						throttling = thr
					}
					if msgs, ok := arg["messages"].([]any); ok {
						for _, mraw := range msgs {
							m, ok := mraw.(map[string]any)
							if !ok {
								continue
							}
							author, _ := m["author"].(string)
							text, _ := m["text"].(string)
							mt, _ := m["messageType"].(string)
							if origin, ok := m["contentOrigin"].(string); ok && origin != "" {
								lastContentOrigin = origin
							}
							if author == "bot" && mt == "" && text != "" {
								// ChatHub often sends the first visible text as a full snapshot,
								// followed by cursor deltas. Emit only the unseen suffix.
								if err := emitSnapshot(text); err != nil {
									return abort(err)
								}
							}
						}
					}
				}
				continue
			}

			if int(t) == 2 {
				item, _ := obj["item"].(map[string]any)
				if item != nil {
					if thr, ok := item["throttling"]; ok {
						throttling = thr
					}
					if res, ok := item["result"].(map[string]any); ok {
						rawResult, _ = res["value"].(string)
						if msg, ok := res["message"].(string); ok {
							final = msg
						}
						// A throttled result frame means nothing useful follows;
						// fail fast with a structured error instead of waiting
						// for the completion frame.
						if throttleSignal(rawResult) {
							return Result{}, &ThrottleError{Value: rawResult}
						}
					}
				}
				// completion frame often follows; keep reading a bit but we already have content
				continue
			}

			if int(t) == 3 {
				if errObj, ok := obj["error"].(map[string]any); ok {
					msg, _ := errObj["message"].(string)
					if msg == "" {
						if b, merr := json.Marshal(errObj); merr == nil {
							msg = string(b)
						}
					}
					if throttleSignal(msg) {
						return Result{}, &ThrottleError{Value: msg}
					}
					return Result{}, fmt.Errorf("chathub completion error: %v", errObj)
				}
				// A disengaged turn with no streamed text and no final
				// message carries nothing usable; surface it as a
				// structured failure rather than an empty success.
				if disengagedSeen && streamed.Len() == 0 && strings.TrimSpace(final) == "" {
					return Result{}, &DisengagedError{Message: "no content produced"}
				}
				// end of stream
				log.Printf("chathub timing completion_frame_ms=%d streamed_text=%d events=%d", time.Since(payloadSentAt).Milliseconds(), streamed.Len(), len(events))
				text := final
				if text == "" {
					text = strings.Join(deltas, "")
				}
				return Result{
					Text:           text,
					Reasoning:      reasoningBuf.String(),
					ConversationID: req.ConversationID,
					SessionID:      req.SessionID,
					RequestID:      requestID,
					Throttling:     throttling,
					RawResult:      rawResult,
					ContentOrigin:  lastContentOrigin,
					Events:         events,
					Normalized:     NormalizeEvents(events),
					Images:         imageURLs(events),
				}, nil
			}
		}
	}

	// Reaching the overall deadline without a SignalR completion frame is
	// an incomplete upstream response. Do not return accumulated deltas as if
	// they were a successful, finished answer.
	return Result{}, fmt.Errorf("chathub response deadline exceeded before completion")
}

// Preconn is a pre-dialed, handshake-complete ChatHub connection whose URL
// already carries the account and conversation identifiers of a future chat
// invocation. Consuming it inside ChatWithEventsPreconn/ChatPreconn removes
// TCP+TLS+upgrade+SignalR handshake from the answer's critical path — they
// run while the caller is still busy with tool routing or uploads. The
// connection is single-use: it is closed by the chat that consumes it, or
// explicitly via Close when the answer never happens.
type Preconn struct {
	conn           *websocket.Conn
	acc            Account
	conversationID string
	sessionID      string
	requestID      string
}

// Close releases an unused pre-connection.
func (p *Preconn) Close() { _ = p.conn.Close() }

// ConversationID returns the conversation id baked into the pre-connected URL.
func (p *Preconn) ConversationID() string { return p.conversationID }

// SessionID returns the session id baked into the pre-connected URL.
func (p *Preconn) SessionID() string { return p.sessionID }

// Preconnect dials and completes the SignalR handshake ahead of time. The
// dial is bounded to 10 seconds regardless of ctx so a slow network cannot
// stall a request that could still dial fresh; on any failure the caller
// simply proceeds with a normal Chat.
func (c *Client) Preconnect(ctx context.Context, acc Account, conversationID, sessionID string) (*Preconn, error) {
	if acc.AccessToken == "" || acc.OID == "" || acc.TID == "" {
		return nil, fmt.Errorf("missing access token / oid / tid")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	if conversationID == "" {
		conversationID = uuid.NewString()
	}
	requestID := uuid.NewString()
	wsURL, err := c.buildWSURL(acc, sessionID, conversationID, requestID)
	if err != nil {
		return nil, err
	}
	conn, _, err := c.Dialer.DialContext(dialCtx, wsURL, c.HTTPHeader.Clone())
	if err != nil {
		return nil, fmt.Errorf("ws dial: %w", err)
	}
	if err := signalRHandshake(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Preconn{conn: conn, acc: acc, conversationID: conversationID, sessionID: sessionID, requestID: requestID}, nil
}

// signalRHandshake performs the SignalR JSON protocol handshake and validates
// the server ack. A handshake-level rejection must fail the request up front
// instead of surfacing later as an opaque first-frame read error.
func signalRHandshake(conn *websocket.Conn) error {
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"protocol":"json","version":1}`+rs)); err != nil {
		return fmt.Errorf("handshake send: %w", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("handshake recv: %w", err)
	}
	var ack struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.Trim(string(msg), rs))), &ack); err == nil && ack.Error != "" {
		return fmt.Errorf("handshake rejected: %s", ack.Error)
	}
	return nil
}

// buildWSURL assembles the ChatHub WebSocket URL. WSBase overrides the
// origin so tests can point the client at a fake SignalR server.
func (c *Client) buildWSURL(acc Account, sessionID, conversationID, requestID string) (string, error) {
	q := url.Values{}
	q.Set("chatsessionid", requestID)
	q.Set("clientrequestid", requestID)
	q.Set("X-SessionId", sessionID)
	q.Set("ConversationId", conversationID)
	q.Set("access_token", acc.AccessToken)
	q.Set("variants", variants)
	// source must keep quotes like the browser probe
	q.Set("source", `"officeweb"`)
	q.Set("product", "Office")
	q.Set("agentHost", "Bizchat.FullScreen")
	q.Set("licenseType", "Starter")
	q.Set("agent", "web")
	q.Set("scenario", "OfficeWebIncludedCopilot")

	// url.Values encodes quotes; probe used safe='",' so keep quotes unescaped-ish.
	// Gorilla/url will encode " to %22 which MS accepts.
	base := wsBase
	if c.WSBase != "" {
		base = c.WSBase
	}
	u := fmt.Sprintf("%s/%s@%s?%s", base, acc.OID, acc.TID, q.Encode())
	return u, nil
}

func (c *Client) uploadAttachments(ctx context.Context, acc Account, conversationID string, attachments []Attachment) error {
	imageCount := 0
	// Upload failures are fatal to the request, not silently skipped: a
	// missing DocID means the model never sees the attachment, which used to
	// surface as the model answering "what image?" with no client-visible
	// cause. Collect every failure and abort with an explicit error.
	var uploadErrs []string
	fail := func(format string, args ...any) {
		uploadErrs = append(uploadErrs, fmt.Sprintf("image %d: %s", imageCount, fmt.Sprintf(format, args...)))
	}
	for i := range attachments {
		a := &attachments[i]
		if a.Type != "image" {
			continue
		}
		imageCount++
		if imageCount > maxAttachments {
			return fmt.Errorf("too many image attachments: limit is %d", maxAttachments)
		}
		// For non-data URLs, download the image first
		imageData := a.URL
		if !strings.HasPrefix(a.URL, "data:") {
			if err := validateRemoteDownloadURL(a.URL); err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
			if err != nil {
				fail("build download request: %v", err)
				continue
			}
			resp, err := c.HTTPClient.Do(req)
			if err != nil {
				fail("download: %v", err)
				continue
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentMiB<<20))
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				fail("download status %s", resp.Status)
				continue
			}
			mimeType := resp.Header.Get("Content-Type")
			if mimeType == "" {
				mimeType = "image/png"
			}
			imageData = "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(body)
		}
		comma := strings.IndexByte(imageData, ',')
		if comma < 0 {
			return fmt.Errorf("invalid image data URL")
		}
		encoded := imageData[comma+1:]
		if strings.Contains(strings.ToLower(imageData[:comma]), ";base64") == false {
			return fmt.Errorf("image URL is not base64")
		}
		if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
			return fmt.Errorf("decode image: %w", err)
		}
		form := url.Values{}
		form.Set("scenario", "UploadImage")
		form.Set("conversationId", conversationID)
		// The browser sends the complete data URL in FileBase64, including the
		// media-type prefix. UploadFile accepts this form and returns docId.
		// Live-verified 2026-08-08: UploadFile rejects multipart bodies
		// (HTTP 400 InvalidRequest); it requires x-www-form-urlencoded like
		// PyRIT's httpx client sends.
		form.Set("FileBase64", imageData)
		if c.Trace != nil {
			c.Trace(map[string]any{"stage": "upload_start", "index": i, "conversation_id": conversationID, "mime_type": a.MimeType, "base64_length": len(encoded), "token_present": acc.AccessToken != ""})
		}
		form.Add("optionsSets", "cwcgptvsan")
		form.Add("optionsSets", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://substrate.office.com/m365Copilot/UploadFile", strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if acc.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
		}
		req.Header.Set("Accept", "application/json")
		// Required by the enterprise Copilot UploadFile image-input path.
		// This feature gate is documented in the prior reverse-proxy research
		// and mirrors the PyRIT request flow.
		req.Header.Set("X-Variants", "feature.EnableImageSupportInUploadFile")
		req.Header.Set("X-Scenario", "OfficeWebIncludedCopilot")
		req.Header.Set("Referer", "https://m365.cloud.microsoft/")
		for k, vv := range c.HTTPHeader {
			for _, v := range vv {
				if k != "Origin" || v != "" {
					req.Header.Add(k, v)
				}
			}
		}
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			log.Printf("[upload] http error: %v", err)
			fail("upload http: %v", err)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			log.Printf("[upload] read error: %v", readErr)
			fail("upload read: %v", readErr)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Printf("[upload] status %s: %s", resp.Status, strings.TrimSpace(string(data[:minInt(len(data), 500)])))
			fail("upload status %s", resp.Status)
			continue
		}
		var out struct {
			DocID    string `json:"docId"`
			FileName string `json:"fileName"`
			FileType string `json:"fileType"`
			Result   struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			log.Printf("[upload] json error: %v", err)
			fail("upload response: %v", err)
			continue
		}
		if out.Result.Value != "Success" || out.DocID == "" {
			log.Printf("[upload] failed: %s", strings.TrimSpace(string(data)))
			fail("upload rejected: %s", strings.TrimSpace(string(data[:minInt(len(data), 200)])))
			continue
		}
		a.DocID = out.DocID
		a.FileType = strings.TrimPrefix(strings.ToLower(out.FileType), ".")
		// ChatHub's ImageFile annotation uses jpg for JPEG uploads.
		if a.FileType == "jpeg" {
			a.FileType = "jpg"
		}
		if a.Name == "" {
			a.Name = out.FileName
		}
		if c.Trace != nil {
			c.Trace(map[string]any{"stage": "upload_success", "doc_id": a.DocID, "file_name": a.Name, "file_type": a.FileType})
		}
	}
	if len(uploadErrs) > 0 {
		return fmt.Errorf("attachment upload failed (%d/%d): %s", len(uploadErrs), imageCount, strings.Join(uploadErrs, "; "))
	}
	return nil
}

// threadLevelGptID renders the declarative-agent attachment for the chat
// frame. Empty means no agent: the browser sends an empty object.
func threadLevelGptID(agentGptID string) map[string]any {
	if agentGptID == "" {
		return map[string]any{}
	}
	return map[string]any{"gpts": agentGptID}
}

func chatPayload(text, sessionID, conversationID, requestID, tone string, firstTurn bool, attachments []Attachment, tools []Tool, toolChoice any, mcpServerURL, agentGptID string) string {
	text = toolProtocolPrompt(text, tools, toolChoice)
	message := map[string]any{
		"author":                "user",
		"attachments":           attachments,
		"inputMethod":           "Keyboard",
		"text":                  text,
		"entityAnnotationTypes": []string{"People", "File", "Event", "Email", "TeamsMessage"},
		"requestId":             requestID,
		"locationInfo": map[string]any{
			"timeZoneOffset": 8,
			"timeZone":       "Asia/Shanghai",
		},
		"locale":            "zh-cn",
		"messageType":       "Chat",
		"experienceType":    "Default",
		"adaptiveCards":     []any{},
		"clientPreferences": map[string]any{},
	}
	// The browser does not send an OpenAI attachments array to ChatHub. It
	// sends a file annotation after the file has been uploaded by Office.
	annotations := make([]any, 0, len(attachments))
	for _, a := range attachments {
		if a.Type != "image" || a.DocID == "" {
			continue
		}
		if a.Name == "" {
			a.Name = "image." + a.FileType
		}
		fileType := a.FileType
		if fileType == "" {
			fileType = strings.TrimPrefix(strings.ToLower(a.MimeType), "image/")
		}
		if fileType == "" || fileType == "image" || fileType == "*" {
			fileType = "jpg"
		}
		annotations = append(annotations, map[string]any{
			"id": a.DocID,
			"messageAnnotationMetadata": map[string]any{
				"@type": "File", "annotationType": "File",
				"fileType": fileType, "fileName": a.Name,
			},
			"messageAnnotationType": "ImageFile",
		})
	}
	if len(annotations) > 0 {
		message["messageAnnotations"] = annotations
		message["connectedFederatedConnections"] = []string{"dummyId"}
	}
	// Restore the old gateway's multimodal injection path. The historical
	// implementation merged imageUrl/imageBase64 directly into message rather
	// than relying solely on the newer attachments array.
	for _, a := range attachments {
		if a.Type != "image" || a.URL == "" {
			continue
		}
		if strings.HasPrefix(a.URL, "data:") {
			if comma := strings.IndexByte(a.URL, ','); comma >= 0 && comma+1 < len(a.URL) {
				message["imageBase64"] = a.URL[comma+1:]
			}
		} else {
			message["imageUrl"] = a.URL
		}
		break
	}
	optionsSets := []any{
		"search_result_progress_messages_with_search_queries",
		"update_textdoc_response_after_streaming",
		"deepleo_networking_timeout_10minutes_canmore",
		"cwc_flux_image",
		"cwc_code_interpreter",
		"cwc_code_interpreter_amsfix",
		"cwcfluxgptv",
		"flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch",
		"gptvnorm2048",
		"cwc_code_interpreter_citation_fix",
		"code_interpreter_interactive_charts_inline_image",
		"code_interpreter_matplotlib_patching",
		"code_interpreter_interactive_charts",
		"cwc_fileupload_odb",
		"update_memory_plugin",
		"add_custom_instructions",
		"cwc_flux_v3",
		"flux_v3_progress_messages",
		"enable_batch_token_processing",
		"enable_gg_gpt",
	}
	chat := map[string]any{
		"arguments": []any{
			map[string]any{
				"source":              "officeweb",
				"clientCorrelationId": uuid.NewString(),
				"sessionId":           sessionID,
				"optionsSets":         optionsSets,
				"options":             map[string]any{},
				"allowedMessageTypes": []string{
					"Chat", "Suggestion", "Disengaged", "Progress", "EndOfRequest", "InternalLoaderMessage",
					// Required to receive server-side code interpreter output
					// (cwc_code_interpreter optionsSets are already requested)
					// and live citation frames, mirroring the browser HAR list.
					"GeneratedCode", "SearchResults", "SourceAttributions",
				},
				"sliceIds":          []any{},
				"threadLevelGptId":  threadLevelGptID(agentGptID),
				"conversationId":    conversationID,
				"traceId":           uuid.NewString(),
				"isStartOfSession":  firstTurn,
				"productThreadType": "Office",
				"clientInfo": map[string]any{
					"clientPlatform": "mcmcopilot-web",
					"clientAppName":  "Office",
				},
				"tone":          tone,
				"streamingMode": "ConciseWithPadding",
				"message":       message,

				"plugins":    clientPlugins(tools, mcpServerURL),
				"toolChoice": toolChoice,
			},
		},
		"invocationId": "0",
		"target":       "chat",
		"type":         4,
	}
	metrics := map[string]any{
		"arguments": []any{
			map[string]any{
				"Timestamps": map[string]string{
					"ConnectionStart":       "",
					"UserInputStart":        "",
					"ConnectionEstablished": "",
					"UserInputSubmit":       "",
				},
			},
		},
		"target": "Metrics",
		"type":   1,
	}
	b1, _ := json.Marshal(chat)
	b2, _ := json.Marshal(metrics)
	return string(b1) + rs + string(b2) + rs
}
