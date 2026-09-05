package chathub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)
// fakeHub runs a local SignalR-style WebSocket server speaking just enough of
// the ChatHub protocol to exercise the client end to end: handshake, chat
// invocation frames, update events, result and completion frames.
type fakeHub struct {
	srv   *httptest.Server
	base  string
	mu    sync.Mutex
	conns int
	get   []string // raw frames received from clients
}

func newFakeHub(t *testing.T, serve func(conn *websocket.Conn)) *fakeHub {
	t.Helper()
	h := &fakeHub{}
	// The gateway client sends the real ChatHub Origin header, which never
	// matches the test server host; accept every upgrade.
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		h.mu.Lock()
		h.conns++
		h.mu.Unlock()
		defer conn.Close()
		serve(conn)
	}))
	t.Cleanup(h.srv.Close)
	h.base = "ws" + strings.TrimPrefix(h.srv.URL, "http")
	return h
}

func (h *fakeHub) connectionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns
}

func (h *fakeHub) received() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.get...)
}

func (h *fakeHub) record(msg []byte) {
	for _, part := range strings.Split(string(msg), rs) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		h.mu.Lock()
		h.get = append(h.get, part)
		h.mu.Unlock()
	}
}

func hubHandshake(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	if !strings.Contains(string(msg), `"protocol":"json"`) {
		t.Fatalf("unexpected handshake frame %q", msg)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{}`+rs)); err != nil {
		t.Fatalf("ack handshake: %v", err)
	}
}

// readNonPingFrame waits for the next client frame that is not a SignalR ping.
func readNonPingFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(time.Until(deadline) + 500*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		for _, part := range strings.Split(string(msg), rs) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(part), &obj) != nil {
				continue
			}
			if int(obj["type"].(float64)) == 6 {
				continue
			}
			return obj
		}
	}
}

// sendFrames writes server frames as one SignalR message.
func sendFrames(t *testing.T, conn *websocket.Conn, frames ...map[string]any) {
	t.Helper()
	parts := make([]string, 0, len(frames))
	for _, f := range frames {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, string(b))
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(strings.Join(parts, rs)+rs)); err != nil {
		t.Fatalf("send frames: %v", err)
	}
}

func testClient(base string) *Client {
	c := NewClient()
	c.WSBase = base
	return c
}

func testAccount() Account { return Account{AccessToken: "tok", OID: "oid", TID: "tid"} }

func updateWithMessages(messages ...map[string]any) map[string]any {
	return map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": messages}}}
}

func botText(text, origin string) map[string]any {
	m := map[string]any{"author": "bot", "text": text}
	if origin != "" {
		m["contentOrigin"] = origin
	}
	return m
}

func resultFrame(value string) map[string]any {
	return map[string]any{"type": 2, "item": map[string]any{"result": map[string]any{"value": value, "message": ""}}}
}

func completionFrame() map[string]any { return map[string]any{"type": 3} }

func TestChatEndToEndTextAndReasoning(t *testing.T) {
	hub := newFakeHub(t, func(conn *websocket.Conn) {
		hubHandshake(t, conn)
		readNonPingFrame(t, conn)
		sendFrames(t, conn,
			updateWithMessages(map[string]any{"text": "step one", "contentOrigin": "ChainOfThoughtSummary", "addToChainOfThought": true}),
			updateWithMessages(botText("Hello", "DeepLeo")),
			updateWithMessages(botText("Hello world", "DeepLeo")),
			resultFrame("Success"),
			completionFrame(),
		)
		time.Sleep(100 * time.Millisecond)
	})
	res, err := testClient(hub.base).Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if res.Text != "Hello world" {
		t.Fatalf("text = %q, want %q", res.Text, "Hello world")
	}
	if res.Reasoning != "step one" {
		t.Fatalf("reasoning = %q, want %q", res.Reasoning, "step one")
	}
	if res.ContentOrigin != "DeepLeo" {
		t.Fatalf("contentOrigin = %q, want DeepLeo", res.ContentOrigin)
	}
	if res.ConversationID == "" || res.SessionID == "" {
		t.Fatal("conversation/session ids must be generated and returned")
	}
	if res.RawResult != "Success" {
		t.Fatalf("rawResult = %q", res.RawResult)
	}
}

func TestChatHandshakeRejection(t *testing.T) {
	hub := newFakeHub(t, func(conn *websocket.Conn) {
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"Rejected"}`+rs))
		time.Sleep(100 * time.Millisecond)
	})
	_, err := testClient(hub.base).Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "handshake rejected") {
		t.Fatalf("want handshake rejection error, got %v", err)
	}
}

func TestChatThrottledResultFailsFast(t *testing.T) {
	hub := newFakeHub(t, func(conn *websocket.Conn) {
		hubHandshake(t, conn)
		readNonPingFrame(t, conn)
		sendFrames(t, conn, resultFrame("Throttled"))
		time.Sleep(100 * time.Millisecond)
	})
	_, err := testClient(hub.base).Chat(context.Background(), testAccount(), Request{Text: "hi"})
	var throttleErr *ThrottleError
	if !asThrottle(err, &throttleErr) {
		t.Fatalf("want ThrottleError, got %v", err)
	}
	if throttleErr.Value != "Throttled" {
		t.Fatalf("throttle value = %q", throttleErr.Value)
	}
}

func asThrottle(err error, target **ThrottleError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*ThrottleError); ok {
		*target = e
		return true
	}
	return false
}

func TestChatDisengagedEmptyTurnIsStructuredError(t *testing.T) {
	hub := newFakeHub(t, func(conn *websocket.Conn) {
		hubHandshake(t, conn)
		readNonPingFrame(t, conn)
		sendFrames(t, conn,
			updateWithMessages(map[string]any{"author": "bot", "messageType": "Disengaged", "text": "canned filler"}),
			completionFrame(),
		)
		time.Sleep(100 * time.Millisecond)
	})
	_, err := testClient(hub.base).Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if _, ok := err.(*DisengagedError); !ok {
		t.Fatalf("want DisengagedError, got %v", err)
	}
}

func TestStopFrameSentOnCancel(t *testing.T) {
	type recorded struct {
		mu   sync.Mutex
		set  []string
	}
	got := &recorded{}
	hub := newFakeHub(t, func(conn *websocket.Conn) {
		hubHandshake(t, conn)
		readNonPingFrame(t, conn)
		// Wait for the stop frame the client must send when its context is
		// canceled mid-generation.
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		got.mu.Lock()
		got.set = append(got.set, string(msg))
		got.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := testClient(hub.base).Chat(ctx, testAccount(), Request{Text: "hi"})
	if err == nil {
		t.Fatal("canceled chat must return an error")
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	found := false
	for _, f := range got.set {
		if strings.Contains(f, `"target":"stop"`) && strings.Contains(f, `"invocationId":"0"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("stop frame missing after cancel; received %q", got.set)
	}
}

func TestPreconnectReusesSingleConnection(t *testing.T) {
	hub := newFakeHub(t, func(conn *websocket.Conn) {
		hubHandshake(t, conn)
		chat := readNonPingFrame(t, conn)
		if _, ok := chat["arguments"]; !ok {
			t.Errorf("expected a chat invocation frame, got %v", chat)
		}
		sendFrames(t, conn, updateWithMessages(botText("pong", "DeepLeo")), resultFrame("Success"), completionFrame())
		time.Sleep(100 * time.Millisecond)
	})
	c := testClient(hub.base)
	pre, err := c.Preconnect(context.Background(), testAccount(), "", "")
	if err != nil {
		t.Fatalf("preconnect: %v", err)
	}
	defer pre.Close()
	if pre.ConversationID() == "" || pre.SessionID() == "" {
		t.Fatal("preconnect must generate conversation/session ids")
	}
	res, err := c.ChatPreconn(context.Background(), testAccount(), pre, Request{Text: "hi"})
	if err != nil {
		t.Fatalf("chat on preconnection: %v", err)
	}
	if res.Text != "pong" {
		t.Fatalf("text = %q, want pong", res.Text)
	}
	if res.ConversationID != pre.ConversationID() {
		t.Fatalf("result conversation %q != preconnected %q", res.ConversationID, pre.ConversationID())
	}
	if n := hub.connectionCount(); n != 1 {
		t.Fatalf("connections = %d, want 1 (preconnection must be reused)", n)
	}
}

func TestPreconnectHonorsCallerConversation(t *testing.T) {
	hub := newFakeHub(t, func(conn *websocket.Conn) {
		hubHandshake(t, conn)
		time.Sleep(50 * time.Millisecond)
	})
	pre, err := testClient(hub.base).Preconnect(context.Background(), testAccount(), "conv-123", "sess-456")
	if err != nil {
		t.Fatalf("preconnect: %v", err)
	}
	defer pre.Close()
	if pre.ConversationID() != "conv-123" || pre.SessionID() != "sess-456" {
		t.Fatalf("preconnect ids = %q/%q, want conv-123/sess-456", pre.ConversationID(), pre.SessionID())
	}
}

func TestChatPayloadAgentAttachment(t *testing.T) {
	withAgent := chatPayload("hi", "s", "c", "r", "magic", true, nil, nil, nil, "", "my-agent")
	if !strings.Contains(withAgent, `"gpts":"my-agent"`) {
		t.Fatalf("agent id missing from payload: %s", withAgent)
	}
	plain := chatPayload("hi", "s", "c", "r", "magic", true, nil, nil, nil, "", "")
	if strings.Contains(plain, `"gpts"`) {
		t.Fatalf("plain payload must not carry gpts: %s", plain)
	}
}

func TestClassifyInterpreterAndAttributionFrames(t *testing.T) {
	evs := classifyUpdateMessages([]any{
		map[string]any{"author": "bot", "messageType": "GeneratedCode", "text": "print(1)"},
		map[string]any{"author": "bot", "contentType": "SourceAttributions", "text": "[refs]"},
		map[string]any{"author": "bot", "contentType": "SearchResults", "text": "docs"},
	})
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	if evs[0].Kind != "progress" {
		t.Fatalf("GeneratedCode kind = %q, want progress", evs[0].Kind)
	}
	if evs[1].Kind != "progress" {
		t.Fatalf("SourceAttributions kind = %q, want progress", evs[1].Kind)
	}
	if evs[2].Kind != "text" {
		t.Fatalf("SearchResults kind = %q, want text", evs[2].Kind)
	}
}
