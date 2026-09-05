package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"m365-copilot2api/internal/chathub"
)

// sseRaw writes a pre-formatted SSE payload (": connected", "data: ...",
// "data: [DONE]") with a write deadline.
func sseRaw(ctx context.Context, w http.ResponseWriter, f http.Flusher, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// writeStreamFinishReason emits a terminal OpenAI-compatible chunk with a
// non-null finish_reason before the stream ends, so strict clients do not
// treat an otherwise successful response as incomplete.
func writeStreamFinishReason(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, id, model, reason string) {
	finishChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": reason}}}
	_ = sseRaw(ctx, w, flusher, "data: "+mustJSON(finishChunk)+"\n\n")
}

// sseChunkWriter emits OpenAI chat.completion.chunk SSE frames with a single
// assistant-role lead-in (the first delta carries "role" regardless of
// whether it is content or reasoning), per-write deadlines and flushes.
type sseChunkWriter struct {
	ctx     context.Context
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	model   string
	first   bool
}

func newSSEChunkWriter(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, id, model string) *sseChunkWriter {
	return &sseChunkWriter{ctx: ctx, w: w, flusher: flusher, id: id, model: model, first: true}
}

func (sw *sseChunkWriter) delta(field, part string) error {
	if part == "" {
		return nil
	}
	if err := sw.ctx.Err(); err != nil {
		return err
	}
	delta := map[string]any{field: part}
	if sw.first {
		delta["role"] = "assistant"
		sw.first = false
	}
	chunk := map[string]any{"id": sw.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": sw.model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
	rc := http.NewResponseController(sw.w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(sw.w, "data: %s\n\n", mustJSON(chunk)); err != nil {
		return err
	}
	sw.flusher.Flush()
	return nil
}

// content streams an answer-text delta.
func (sw *sseChunkWriter) content(part string) error { return sw.delta("content", part) }

// reasoning streams a ChainOfThought delta as DeepSeek-style reasoning_content.
func (sw *sseChunkWriter) reasoning(part string) error { return sw.delta("reasoning_content", part) }

// usageChunk emits the terminal usage-only chunk requested via
// stream_options.include_usage (empty choices plus usage totals).
func (sw *sseChunkWriter) usageChunk(promptTokens, completionTokens int64) {
	chunk := map[string]any{"id": sw.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": sw.model, "choices": []any{}, "usage": map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens}}
	rc := http.NewResponseController(sw.w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, _ = fmt.Fprintf(sw.w, "data: %s\n\n", mustJSON(chunk))
	sw.flusher.Flush()
}

// streamTextGate buffers upstream answer text before it reaches the SSE
// stream. It implements every hold that needs lookahead: stop-sequence
// interception, fenced-shell detection for tool-call conversion, fenced-block
// boundaries, and a small rune holdback. In holdAll mode (JSON response
// format) it only accumulates; the normalized document is emitted once the
// stream completes.
type streamTextGate struct {
	emit      func(string) error
	stops     []string
	shellTool string
	holdAll   bool
	stopHold  int
	pending   strings.Builder
	total     strings.Builder
	stopHit   bool
}

func newStreamTextGate(emit func(string) error, stops []string, shellTool string, holdAll bool) *streamTextGate {
	g := &streamTextGate{emit: emit, stops: stops, shellTool: shellTool, holdAll: holdAll}
	// Hold back enough runes to catch a stop sequence before it leaks to the
	// client; 8 covers the fenced-tool heuristic, stop sequences may need more.
	maxStopRunes := 0
	for _, s := range stops {
		if n := utf8.RuneCountInString(s); n > maxStopRunes {
			maxStopRunes = n
		}
	}
	g.stopHold = 8
	if hold := maxStopRunes - 1; hold > g.stopHold {
		g.stopHold = hold
	}
	return g
}

// push ingests one upstream text delta. It returns true when a stop sequence
// fired: the caller should abort the upstream turn; later pushes keep
// swallowing text without re-emitting the prefix.
func (g *streamTextGate) push(delta string) (bool, error) {
	g.total.WriteString(delta)
	g.pending.WriteString(delta)
	if g.holdAll {
		return false, nil
	}
	if g.stopHit {
		return true, nil
	}
	v := g.pending.String()
	if len(g.stops) > 0 {
		if idx, hit := findStopSequence(v, g.stops); hit {
			if err := g.emit(v[:idx]); err != nil {
				return false, err
			}
			g.stopHit = true
			return true, nil
		}
	}
	// Hold text only while it may still become a shell tool call that needs
	// the complete fenced block to parse, and only when the client actually
	// declared a shell tool. Freezing every answer containing a bash block or
	// the substring "command" stalled coding sessions until end of stream;
	// fencedToolCalls converts shell fences and bare command JSON only for
	// declared shell tools, so without one there is nothing to wait for.
	if g.shellTool != "" &&
		(strings.Contains(v, "```bash") || strings.Contains(v, "```sh") ||
			strings.Contains(v, "```shell") || strings.Contains(v, "```powershell") ||
			strings.Contains(v, "```cmd")) {
		return false, nil
	}
	if i := strings.Index(v, "```"); i >= 0 {
		if err := g.emit(v[:i]); err != nil {
			return false, err
		}
		g.pending.Reset()
		g.pending.WriteString(v[i:])
		return false, nil
	}
	if runeCount := utf8.RuneCountInString(v); runeCount > g.stopHold {
		cut := 0
		seen := 0
		for i := range v {
			if seen == runeCount-g.stopHold {
				cut = i
				break
			}
			seen++
		}
		if err := g.emit(v[:cut]); err != nil {
			return false, err
		}
		g.pending.Reset()
		g.pending.WriteString(v[cut:])
	}
	return false, nil
}

// text returns the full answer text accumulated so far.
func (g *streamTextGate) text() string { return g.total.String() }

// pendingText returns the text still held back from the stream.
func (g *streamTextGate) pendingText() string { return g.pending.String() }

// ingestFallback records a final-result text recovery: some ChatHub updates
// carry the whole answer only in the completion frame, never as deltas.
func (g *streamTextGate) ingestFallback(s string) {
	g.total.WriteString(s)
	g.pending.WriteString(s)
}

// setM365ResponseHeaders exposes raw ChatHub metadata as response headers for
// non-stream calls. Streaming responses cannot set headers after the body has
// started, so they emit an SSE comment instead (m365StreamComment).
func setM365ResponseHeaders(w http.ResponseWriter, res chathub.Result) {
	if res.ConversationID != "" {
		w.Header().Set("X-M365-Conversation-Id", res.ConversationID)
	}
	if res.RequestID != "" {
		w.Header().Set("X-M365-Request-Id", res.RequestID)
	}
	if res.RawResult != "" {
		w.Header().Set("X-M365-Result", res.RawResult)
	}
	if res.Throttling != nil {
		if b, err := json.Marshal(res.Throttling); err == nil {
			w.Header().Set("X-M365-Throttling", string(b))
		}
	}
}

// m365StreamComment renders terminal ChatHub metadata as an SSE comment line.
// Comments are ignored by every conforming event-source parser, so this adds
// observability without breaking strict clients.
func m365StreamComment(res chathub.Result) string {
	var parts []string
	if res.RawResult != "" {
		parts = append(parts, `"result":`+mustJSON(res.RawResult))
	}
	if res.ConversationID != "" {
		parts = append(parts, `"conversationId":`+mustJSON(res.ConversationID))
	}
	if len(parts) == 0 {
		return ""
	}
	return ": m365 {" + strings.Join(parts, ",") + "}\n\n"
}
