package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/chathub"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *Server) openaiChat(w http.ResponseWriter, r *http.Request) {
	requestID := requestIDFrom(r)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	startedAt := time.Now()
	log.Printf("[req-trace] id=%s stage=http_start stream=%t", requestID, r.URL.Query().Get("stream") == "true")
	defer func() {
		log.Printf("[req-trace] id=%s stage=http_return total_ms=%d", requestID, time.Since(startedAt).Milliseconds())
	}()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const maxChatRequestBody = 10 << 20
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var body oaiReq
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	responseFormat := body.ResponseFormat
	effort := body.ReasoningEffort
	if body.Reasoning != nil && strings.TrimSpace(body.Reasoning.Effort) != "" {
		effort = body.Reasoning.Effort
	}
	tone, toneErr := reasoningTone(body.Model, effort)
	if toneErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", toneErr.Error())
		return
	}
	normalizeLegacyTools(&body)
	ensureWebSearchEnabled(&body)
	body.ConversationID = firstNonEmpty(body.ConversationID, body.ConversationIDC)
	body.SessionID = firstNonEmpty(body.SessionID, body.SessionIDC)
	sampling, err := normalizeSamplingParams(&body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	log.Printf("[req-trace] id=%s stage=body_parsed messages=%d tools=%d choice=%s raw_bytes=%d", requestID, len(body.Messages), len(body.Tools), normalizedToolChoiceMode(body.ToolChoice), len(raw))
	if err := validateToolConversation(body.Messages); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "tool_protocol_error", err.Error())
		return
	}
	// Rebuild a protocol-neutral evidence ledger from actual tool calls/results.
	// Round limits apply only to the current user turn; full history still informs evidence.
	ledger := buildAgentLedger(body.Messages)
	activeLedger := buildAgentLedger(activeMessages(body.Messages))
	if err := activeLedger.CanContinue(maxToolRounds()); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "tool_round_limit", "message": err.Error(), "completed_calls": len(activeLedger.Completed)}})
		return
	}
	// Preserve role boundaries when adapting OpenAI messages to ChatHub's
	// single message.text field. This keeps system/developer instructions,
	// history, and the current user turn distinguishable.
	var prompt string
	prompt, body.Attachments = flattenPromptMessages(body.Messages, body.Attachments)
	log.Printf("[req-trace] id=%s stage=prompt_flattened prompt_len=%d attachments=%d", requestID, len(prompt), len(body.Attachments))
	fmt.Printf("[multimodal-entry] messages=%d attachments=%d prompt_len=%d\n", len(body.Messages), len(body.Attachments), len(prompt))
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		http.Error(w, "messages required", http.StatusBadRequest)
		return
	}

	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	if body.User != "" && body.ConversationID == "" {
		if us, ok := s.userSessions.Get(body.User); ok {
			body.AccountID = firstNonEmpty(body.AccountID, us.AccountID)
			body.ConversationID = us.ConversationID
			body.SessionID = us.SessionID
			log.Printf("[user-session] hit user=%s conversation=%s session=%s", body.User, us.ConversationID, us.SessionID)
		}
	}
	// 内容键会话复用：命中后云端对话已存全量历史，只需把客户端新增的
	// 消息拼成增量 prompt 发送（对齐 DeepSeek 上下文缓存语义）。
	answerPrompt := prompt
	if body.ConversationID == "" && len(body.Messages) > 0 {
		resolved := s.sessionResolver.Resolve(r, &body)
		if !resolved.IsNew {
			body.ConversationID = resolved.ConversationID
			body.SessionID = resolved.SessionID
			body.AccountID = firstNonEmpty(body.AccountID, resolved.AccountID)
			log.Printf("[session-resolver] matched=%s conversation=%s history=%d total=%d", resolved.MatchedBy, resolved.ConversationID, resolved.HistoryLen, len(body.Messages))
			if resolved.HistoryLen > 0 && resolved.HistoryLen < len(body.Messages) {
				incPrompt, incAtt := flattenPromptMessages(body.Messages[resolved.HistoryLen:], nil)
				incPrompt = strings.TrimSpace(incPrompt)
				if incPrompt != "" {
					answerPrompt = incPrompt
					body.Attachments = incAtt
				}
			}
		}
	}
	accountID := body.AccountID
	acc, err := s.resolveAccount(accountID)
	if err != nil {
		log.Printf("[account-route] resolve failed requested=%q err=%v", accountID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[account-route] selected id=%q email=%q token_present=%t oid_present=%t tid_present=%t", acc.ID, acc.Email, acc.AccessToken != "", acc.OID != "", acc.TID != "")
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid", http.StatusBadRequest)
		return
	}

	// Normalize tools once. Selection is always made by the upstream model;
	// the gateway only validates its structured decision and converts protocols.
	toolMaps := make([]map[string]any, 0, len(body.Tools))
	for _, tool := range body.Tools {
		var f map[string]any
		_ = json.Unmarshal(tool.Function, &f)
		toolMaps = append(toolMaps, map[string]any{"type": tool.Type, "function": f})
	}
	routeMaps := routeableTools(toolMaps)
	if body.ToolChoice == nil && len(toolMaps) > 0 {
		body.ToolChoice = "auto"
	}
	planningMode := s.settings.get().ToolPlanningMode

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}
	// Pre-generate conversation identifiers for a new conversation so the
	// answer-turn socket can be pre-dialled while the router conversation or
	// attachment uploads are still in flight.
	isNewConversation := body.ConversationID == ""
	if isNewConversation {
		body.ConversationID = uuid.NewString()
		if body.SessionID == "" {
			body.SessionID = uuid.NewString()
		}
	}
	// A declarative agent overrides the upstream tone unconditionally, so it
	// only rides on tool-bearing answer turns.
	agentGptID := ""
	if len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		agentGptID = s.settings.get().AgentGptID
	}
	// Preconnect is best-effort and only worth it when real work (a router
	// round trip or attachment uploads) overlaps the dial; otherwise the chat
	// dials first anyway. On any failure the answer dials fresh.
	shouldPreconnect := (planningMode == "router" && len(routeMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none") || len(body.Attachments) > 0
	var preconn *chathub.Preconn
	if shouldPreconnect {
		p, preconnErr := s.chat.Preconnect(ctx, account, body.ConversationID, body.SessionID)
		if preconnErr != nil {
			log.Printf("[preconnect] fallback to fresh dial: %v", preconnErr)
		}
		preconn = p
	}
	consumePreconn := func() *chathub.Preconn {
		p := preconn
		preconn = nil
		return p
	}
	defer func() {
		// Release an unconsumed pre-connection; a consumed one is closed by
		// the chat that ran on it.
		if preconn != nil {
			preconn.Close()
			preconn = nil
		}
	}()
	// The stream is opened by the actual response path below. Do not emit a
	// tool preamble here: a request may contain tools in its schema while still
	// being an ordinary text question.
	if planningMode == "router" && body.Stream && len(routeMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		// Preserve the existing validated tool router for streaming tool turns.
		// Only fall through to text streaming when the router explicitly selects
		// no tool; this prevents a natural-language preamble from becoming a
		// completed assistant turn with the actual call lost.
		calls, routeRes, parsed, routeErr := s.runToolRouter(ctx, account, tone, requestID, prompt+"\n"+ledger.RouterContext(), routeMaps, body.ToolChoice, body.Attachments, ledger)
		log.Printf("[req-trace] id=%s stage=router_return elapsed_ms=%d err=%t", requestID, time.Since(startedAt).Milliseconds(), routeErr != nil)
		if routeErr != nil {
			http.Error(w, routeErr.Error(), http.StatusBadGateway)
			return
		}
		if parsed && len(calls) > 0 {
			_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), true, s.finalizeToolCalls(calls, fmt.Sprintf("%d:%v:stream", len(body.Messages), completedCallIDs(ledger))), routeRes)
			return
		}
	}
	if body.Stream {
		answerPrompt = answerPrompt + "\n" + ledger.RouterContext() + "\nFINAL ANSWER RULE: Answer the user directly. If a tool is explicitly required, emit its structured call; otherwise return ordinary text."
		log.Printf("[req-trace] id=%s stage=answer_start prompt_len=%d", requestID, len(answerPrompt))
		answerReq := chathub.Request{Text: answerPrompt, Tone: tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments, Tools: body.Tools, ToolChoice: body.ToolChoice, Started: isNewConversation, AgentGptID: agentGptID}
		id := "chatcmpl-" + uuid.NewString()
		model := firstNonEmpty(body.Model, "m365-copilot")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		if err := sseRaw(r.Context(), w, flusher, ": connected\n\n"); err != nil {
			return
		}
		var streamedTools []detectedToolCall
		// Streaming text is only held back for possible fenced tool calls when
		// the client actually declared a shell tool; otherwise every byte
		// streams as soon as it arrives. JSON mode buffers everything so the
		// normalized document can be emitted once at stream end.
		jsonMode := responseFormat != nil && (responseFormat.Type == "json_object" || responseFormat.Type == "json_schema")
		shellTool := declaredShell(allowedToolNames(toolMaps))
		lengthCapped := false
		stopHit := false
		sw := newSSEChunkWriter(r.Context(), w, flusher, id, model)
		gate := newStreamTextGate(sw.content, sampling.stops, shellTool, jsonMode)
		res, err := s.chat.ChatWithEventsPreconn(ctx, account, consumePreconn(), answerReq, func(ev chathub.StreamEvent) error {
			if ev.Kind == "tool" && ev.ToolName != "" && len(ev.Arguments) > 0 {
				streamedTools = append(streamedTools, detectedToolCall{ID: "call_" + uuid.NewString(), Name: ev.ToolName, Arguments: ev.Arguments})
				return nil
			}
			// ChainOfThought deltas stream as reasoning_content (DeepSeek-style)
			// ahead of the answer text; strict OpenAI clients ignore the field.
			if ev.Kind == "reasoning" && ev.Text != "" {
				return sw.reasoning(ev.Text)
			}
			if ev.Kind != "text" || ev.Text == "" {
				return nil
			}
			hit, err := gate.push(ev.Text)
			if err != nil {
				return err
			}
			if hit {
				stopHit = true
				// cancel() aborts the upstream turn through the chathub
				// stop-frame path; the post-stream handler treats this as a
				// clean finish. The gate swallows further deltas without
				// re-emitting the intercepted prefix.
				cancel()
				return nil
			}
			if sampling.maxTokens > 0 && EstimateTokens(gate.text()) >= sampling.maxTokens {
				lengthCapped = true
				cancel()
			}
			return nil
		})
		if err != nil && (stopHit || lengthCapped) && errors.Is(err, context.Canceled) {
			// Deliberate local abort through cancel(): the chathub layer already
			// sent the upstream stop frame, so finish cleanly instead of
			// reporting an error to the client.
			err = nil
		}
		if err != nil {
			log.Printf("[req-trace] id=%s stage=stream_error err=%v", requestID, err)
			s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
			msg := upstreamError(err)
			if IsRateLimited(err) {
				msg = "upstream is rate limiting; try again shortly"
			}
			_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": msg, "code": upstreamErrorCode(err)}})+"\n\n")
			_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
			return
		}
		s.accountPool.MarkSuccess(acc.ID)
		// Some ChatHub updates contain no text event and place the completed
		// answer only in the final Result. Recover it before deciding that the
		// response is empty; this also preserves fenced-tool parsing.
		if gate.text() == "" && strings.TrimSpace(res.Text) != "" {
			gate.ingestFallback(res.Text)
		}
		calls := streamedTools
		if len(calls) == 0 {
			calls = fencedToolCalls(gate.text(), toolMaps, body.ToolChoice)
		}
		if len(calls) > 0 {
			calls = s.finalizeToolCalls(calls, "")
			_ = writeToolResponse(w, id, model, true, calls, chathub.Result{Text: gate.text()})
			if body.User != "" && res.ConversationID != "" {
				s.userSessions.Put(body.User, res.ConversationID, res.SessionID, acc.ID)
			}
			s.bindConversation(acc, &body, r, res, answerPrompt, startedAt)
			return
		}
		finalText := gate.pendingText()
		finish := "stop"
		if jsonMode {
			finalText = gate.text()
			if idx, hit := findStopSequence(finalText, sampling.stops); hit {
				finalText = finalText[:idx]
			}
			if sampling.maxTokens > 0 {
				var truncated bool
				if finalText, truncated = truncateToTokens(finalText, sampling.maxTokens); truncated {
					finish = "length"
				}
			}
			finalText = normalizeJSONText(finalText)
		} else if lengthCapped {
			finish = "length"
		} else if stopHit {
			// Everything up to the stop sequence was already emitted.
			finalText = ""
		}
		if finalText != "" {
			if err := sw.content(finalText); err != nil {
				log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
				return
			}
		}
		writeStreamFinishReason(r.Context(), w, flusher, id, model, finish)
		if meta := m365StreamComment(res); meta != "" {
			_ = sseRaw(r.Context(), w, flusher, meta)
		}
		if sampling.includeUsage {
			sw.usageChunk(int64(EstimateTokens(answerPrompt)), int64(EstimateTokens(gate.text())))
		}
		_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
		if body.User != "" && res.ConversationID != "" {
			s.userSessions.Put(body.User, res.ConversationID, res.SessionID, acc.ID)
		}
		s.bindConversation(acc, &body, r, res, answerPrompt, startedAt)
		return
	}
	// Ask the upstream model to select and validate the next tool. The gateway
	// remains tool-agnostic; it only validates and serializes the decision.
	if planningMode == "router" && len(routeMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		calls, routeRes, parsed, routeErr := s.runToolRouter(ctx, account, tone, requestID, prompt+"\n"+ledger.RouterContext(), routeMaps, body.ToolChoice, body.Attachments, ledger)
		if routeErr != nil {
			http.Error(w, routeErr.Error(), http.StatusBadGateway)
			return
		}
		if parsed && len(calls) > 0 {
			_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), body.Stream, s.finalizeToolCalls(calls, fmt.Sprintf("%d:%v", len(body.Messages), completedCallIDs(ledger))), routeRes)
			return
		}
		if fmt.Sprint(body.ToolChoice) == "required" {
			defs, _ := json.Marshal(routeMaps)
			retryText := `Select at least one required next tool call from FUNCTION_DEFINITIONS. Validate every argument against its schema. Return JSON only as {"calls":[{"name":"function_name","arguments":{}}]}.
APPLICATION_REQUEST_AND_EVIDENCE:
` + prompt + "\n" + ledger.RouterContext() + "\nFUNCTION_DEFINITIONS:\n" + string(defs)
			retryRes, retryErr := s.chat.Chat(ctx, account, chathub.Request{Text: retryText, Tone: tone, Attachments: body.Attachments})
			if retryErr == nil {
				calls, parsed = parseModelToolDecision(retryRes.Text, routeMaps, body.ToolChoice)
				calls = filterCompletedCalls(calls, ledger)
				if parsed && len(calls) > 0 {
					_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), body.Stream, s.finalizeToolCalls(calls, fmt.Sprintf("%d:%v:required-retry", len(body.Messages), completedCallIDs(ledger))), retryRes)
					return
				}
			}
			http.Error(w, "model did not select a required tool after constrained retry", http.StatusBadGateway)
			return
		}
	}
	if len(ledger.Completed) > 0 || len(ledger.Pending) > 0 {
		answerPrompt += "\n" + ledger.RouterContext()
	}
	if len(ledger.Completed) > 0 {
		answerPrompt += "\nFINAL ANSWER RULE: Report only actions supported by completed tool results. If the goal is not fully verified, state exactly what remains unconfirmed."
	}
	answerReq := chathub.Request{Text: answerPrompt, Tone: tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments, Started: isNewConversation, AgentGptID: agentGptID}
	if planningMode == "native" {
		answerReq.Tools = body.Tools
		answerReq.ToolChoice = body.ToolChoice
	}
	res, err := s.chat.ChatPreconn(ctx, account, consumePreconn(), answerReq)
	if err != nil && body.AccountID == "" && isNewConversation && (IsRateLimited(err) || IsAuthFailure(err)) {
		// Failover only when nothing pins the request to a conversation or
		// account; a fresh chat can safely retry on the next healthy account.
		// The retry regenerates identifiers on the new account, mirroring a
		// conversation that never started.
		next, nerr := s.nextHealthyAccount(acc.ID)
		if nerr == nil {
			ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
			defer cancel2()
			retryReq := answerReq
			retryReq.ConversationID = ""
			retryReq.SessionID = ""
			retryReq.Started = false
			res2, err2 := s.chat.Chat(ctx2, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, retryReq)
			if err2 == nil {
				res = res2
				acc = next
				err = nil
				s.accountPool.MarkSuccess(next.ID)
			}
		}
	}
	if err != nil {
		s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
		writeUpstreamError(w, err)
		return
	}

	if body.SessionKey != "" {
		s.sessions.upsert(conversation{ID: body.SessionKey, AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID, Title: prompt})
	}
	if body.User != "" && res.ConversationID != "" {
		s.userSessions.Put(body.User, res.ConversationID, res.SessionID, acc.ID)
		log.Printf("[user-session] put user=%s conversation=%s session=%s", body.User, res.ConversationID, res.SessionID)
	}
	if res.ConversationID != "" {
		s.bindConversation(acc, &body, r, res, prompt, startedAt)
	}
	if res.ConversationID != "" {
		resolved := s.sessionResolver.Resolve(r, &body)
		if !resolved.IsNew {
			w.Header().Set(sessionHeaderName, resolved.SessionID)
		}
	}
	model := body.Model
	if model == "" {
		model = "m365-copilot"
	}
	id := "chatcmpl-" + uuid.NewString()
	if calls := fencedToolCalls(res.Text, toolMaps, body.ToolChoice); len(calls) > 0 {
		calls = s.finalizeToolCalls(calls, "")
		_ = writeToolResponse(w, id, model, body.Stream, calls, res)
		return
	}
	if calls := nativeToolCalls(res.Events, body.Tools); len(calls) > 0 {
		calls = s.finalizeToolCalls(calls, "")
		_ = writeToolResponse(w, id, model, body.Stream, calls, res)
		return
	}
	// Recover natural-language tool intent when native mode emits no
	// structured ChatHub tool event. Plain text remains a zero-call result.
	if planningMode == "native" && len(routeMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		calls, routeRes, parsed, routeErr := s.runToolRouter(ctx, account, tone, requestID, prompt+"\n"+ledger.RouterContext(), routeMaps, body.ToolChoice, body.Attachments, ledger)
		if routeErr == nil && parsed && len(calls) > 0 {
			_ = writeToolResponse(w, id, model, body.Stream, s.finalizeToolCalls(calls, fmt.Sprintf("%d:%v:native-recovery", len(body.Messages), completedCallIDs(ledger))), routeRes)
			return
		}
	}
	if !completionEvidenceAllows(res.Text, ledger) {
		res.Text = "I cannot confirm completion because no matching tool results were returned. No external action has been verified."
	}
	log.Printf("[debug] res.Text bytes=%d content=%q", len(res.Text), res.Text)
	created := time.Now().Unix()

	finish := "stop"
	if responseFormat != nil && (responseFormat.Type == "json_object" || responseFormat.Type == "json_schema") {
		res.Text = normalizeJSONText(res.Text)
	}
	if idx, hit := findStopSequence(res.Text, sampling.stops); hit {
		res.Text = res.Text[:idx]
	}
	if sampling.maxTokens > 0 {
		var truncated bool
		if res.Text, truncated = truncateToTokens(res.Text, sampling.maxTokens); truncated {
			finish = "length"
		}
	}
	content := any(res.Text)
	if len(res.Images) > 0 {
		parts := []any{map[string]any{"type": "text", "text": res.Text}}
		for _, u := range res.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
		}
		content = parts
	}
	assistant := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if res.Reasoning != "" {
		assistant["reasoning_content"] = res.Reasoning
	}
	// 上游 ChatHub 不返回 token 计数，按请求/回复文本本地估算填充
	// OpenAI 要求的 usage 字段。
	pt := EstimateTokens(prompt)
	ct := EstimateTokens(res.Text)
	setM365ResponseHeaders(w, res)
	jsonOut(w, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       assistant,
			"finish_reason": finish,
		}},
		"m365": compatM365Metadata(res),
		"usage": map[string]any{
			"prompt_tokens":     pt,
			"completion_tokens": ct,
			"total_tokens":      pt + ct,
		},
	})
}
