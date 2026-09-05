package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// streamAnthropicAdapter runs the internal OpenAI streaming path and converts
// its SSE chunks into Anthropic Messages SSE events incrementally. This gives
// /v1/messages true streaming — first-byte latency tracks the first upstream
// token — instead of the previous aggregate-then-replay behavior that made
// every streamed answer arrive in one burst after full generation.
//
// Returns the accumulated output text and the number of tool_call deltas so
// the caller can record usage; ok is false when the stream ended in an error
// (an Anthropic "error" event has already been emitted in that case).
func (s *Server) streamAnthropicAdapter(w http.ResponseWriter, r *http.Request, o oaiReq, model string) (string, int, bool) {
	o.Stream = true
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	pr, pw := io.Pipe()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	innerDone := make(chan struct{})
	go func() {
		defer close(innerDone)
		s.openaiChat(irw, r2)
		_ = pw.Close()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)

	inputEstimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, "")
	inputTokens, _ := inputEstimate.Values["input_tokens"].(int)

	id := "msg_" + uuid.NewString()
	aborted := false
	emit := func(name string, v any) {
		if aborted {
			return
		}
		if err := sseWriteFrame(w, flusher, name, v); err != nil {
			aborted = true
		}
	}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
	}})

	var text strings.Builder
	var outputTokens int
	var toolCount int
	stopReason := "end_turn"

	// Content block state machine. Anthropic requires exactly one open block;
	// blocks open lazily on the first delta of their kind and close when a
	// different kind starts or the stream ends.
	nextIndex := 0
	curIndex := -1
	curKind := ""
	curToolIdx := -1
	closeBlock := func() {
		if curKind != "" {
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": curIndex})
			curKind = ""
			curIndex = -1
			curToolIdx = -1
		}
	}
	openBlock := func(kind string, block map[string]any) {
		closeBlock()
		curKind = kind
		curIndex = nextIndex
		nextIndex++
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": curIndex, "content_block": block})
	}

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		if r.Context().Err() != nil || aborted {
			break
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if errObj, ok := chunk["error"].(map[string]any); ok {
			msg, _ := errObj["message"].(string)
			if msg == "" {
				msg = "upstream request failed"
			}
			closeBlock()
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": anthropicErrorType(errObj), "message": msg}})
			<-innerDone
			return text.String(), toolCount, false
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			// Terminal usage-only chunk (stream_options.include_usage).
			if usage, ok := chunk["usage"].(map[string]any); ok {
				if ct, ok := usage["completion_tokens"].(float64); ok {
					outputTokens = int(ct)
				}
			}
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
			if curKind != "thinking" {
				openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
			}
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": curIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": reasoning}})
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			if curKind != "text" {
				openBlock("text", map[string]any{"type": "text", "text": ""})
			}
			text.WriteString(content)
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": curIndex, "delta": map[string]any{"type": "text_delta", "text": content}})
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range rawCalls {
				tc, _ := raw.(map[string]any)
				idxF, _ := tc["index"].(float64)
				toolIdx := int(idxF)
				fn, _ := tc["function"].(map[string]any)
				if curKind != "tool_use" || curToolIdx != toolIdx {
					callID, _ := tc["id"].(string)
					name, _ := fn["name"].(string)
					if callID == "" {
						callID = "call_" + uuid.NewString()
					}
					curToolIdx = toolIdx
					openBlock("tool_use", map[string]any{"type": "tool_use", "id": callID, "name": name, "input": map[string]any{}})
					toolCount++
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": curIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
				}
			}
		}
		if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
			switch finish {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			default:
				stopReason = "end_turn"
			}
		}
	}
	<-innerDone
	if aborted {
		return text.String(), toolCount, false
	}
	if irw.status >= http.StatusBadRequest {
		// openaiChat answered with a plain HTTP error (no SSE error chunk).
		closeBlock()
		emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "inner chat request failed"}})
		return text.String(), toolCount, false
	}
	closeBlock()
	if curKind == "" && nextIndex == 0 {
		// Anthropic clients expect at least one content block.
		openBlock("text", map[string]any{"type": "text", "text": ""})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": curIndex, "delta": map[string]any{"type": "text_delta", "text": ""}})
	}
	closeBlock()
	if outputTokens == 0 {
		outputTokens = int(EstimateTokens(text.String()))
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": outputTokens}})
	emit("message_stop", map[string]any{"type": "message_stop"})
	return text.String(), toolCount, true
}

// anthropicErrorType maps a gateway error code to the closest Anthropic error
// type for streamed error events.
func anthropicErrorType(errObj map[string]any) string {
	code, _ := errObj["code"].(string)
	switch code {
	case "rate_limit":
		return "rate_limit_error"
	case "upstream_auth":
		return "authentication_error"
	default:
		return "api_error"
	}
}

// anthropicCountTokens implements POST /v1/messages/count_tokens with the
// same estimator the usage accounting uses; ChatHub exposes no counting
// endpoint upstream.
func (s *Server) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, "")
	jsonOut(w, map[string]any{"input_tokens": estimate.Values["input_tokens"]})
}
