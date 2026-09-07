package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordedDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	Reason  string `json:"reasoning_content,omitempty"`
}

type recordedChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int           `json:"index"`
		Delta        recordedDelta `json:"delta"`
		FinishReason *string       `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

func parseSSEChunks(t *testing.T, body string) []recordedChunk {
	t.Helper()
	var out []recordedChunk
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var c recordedChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", line, err)
		}
		out = append(out, c)
	}
	return out
}

func TestSSEChunkWriterRoleLeadsFirstDelta(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEChunkWriter(context.Background(), rec, rec, "chatcmpl-x", "m")
	if err := sw.reasoning("thinking"); err != nil {
		t.Fatalf("reasoning: %v", err)
	}
	if err := sw.content("Hello"); err != nil {
		t.Fatalf("content: %v", err)
	}
	chunks := parseSSEChunks(t, rec.Body.String())
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Reason != "thinking" || chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first delta = %+v, want role+reasoning", chunks[0].Choices[0].Delta)
	}
	if chunks[1].Choices[0].Delta.Content != "Hello" || chunks[1].Choices[0].Delta.Role != "" {
		t.Fatalf("second delta = %+v, want content without role", chunks[1].Choices[0].Delta)
	}
	if chunks[1].Model != "m" || chunks[1].ID != "chatcmpl-x" || chunks[1].Object != "chat.completion.chunk" {
		t.Fatalf("chunk envelope = %+v", chunks[1])
	}
}

func TestSSEChunkWriterUsageChunk(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEChunkWriter(context.Background(), rec, rec, "chatcmpl-x", "m")
	sw.usageChunk(2, 3)
	chunks := parseSSEChunks(t, rec.Body.String())
	if len(chunks) != 1 || chunks[0].Usage == nil {
		t.Fatalf("usage chunk missing: %+v", chunks)
	}
	if len(chunks[0].Choices) != 0 {
		t.Fatalf("usage chunk must carry empty choices")
	}
	if chunks[0].Usage.PromptTokens != 2 || chunks[0].Usage.CompletionTokens != 3 || chunks[0].Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v", chunks[0].Usage)
	}
}

func TestSSEChunkWriterEmptyPartEmitsNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEChunkWriter(context.Background(), rec, rec, "x", "m")
	if err := sw.content(""); err != nil {
		t.Fatalf("content(empty): %v", err)
	}
	if err := sw.reasoning(""); err != nil {
		t.Fatalf("reasoning(empty): %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("empty parts must not write, got %q", rec.Body.String())
	}
}

func TestStreamTextGateStreamsTailAfterHoldback(t *testing.T) {
	var emitted []string
	g := newStreamTextGate(func(s string) error { emitted = append(emitted, s); return nil }, nil, "", false)
	if _, err := g.push("Hello"); err != nil {
		t.Fatal(err)
	}
	if len(emitted) != 0 {
		t.Fatalf("short text must stay held, emitted %q", emitted)
	}
	if _, err := g.push(" world"); err != nil {
		t.Fatal(err)
	}
	if len(emitted) != 1 {
		t.Fatalf("expected one tail emission, got %q", emitted)
	}
	if g.text() != "Hello world" {
		t.Fatalf("text = %q", g.text())
	}
	if g.pendingText() != "lo world" {
		t.Fatalf("pending = %q, want held tail", g.pendingText())
	}
	if emitted[0] != "Hel" {
		t.Fatalf("emitted = %q, want Hel", emitted[0])
	}
}

func TestStreamTextGateStopSequenceInterceptsOnce(t *testing.T) {
	var emitted []string
	g := newStreamTextGate(func(s string) error { emitted = append(emitted, s); return nil }, []string{"STOP"}, "", false)
	if _, err := g.push("abc"); err != nil {
		t.Fatal(err)
	}
	hit, err := g.push("DESTOPmore")
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("stop sequence must fire")
	}
	if len(emitted) != 1 || emitted[0] != "abcDE" {
		t.Fatalf("emitted = %q, want [abcDE]", emitted)
	}
	// Later deltas must keep swallowing without re-emitting the prefix.
	for i := 0; i < 3; i++ {
		hit, err := g.push("more")
		if err != nil {
			t.Fatal(err)
		}
		if !hit {
			t.Fatal("stop state must persist")
		}
	}
	if len(emitted) != 1 {
		t.Fatalf("duplicate emission after stop: %q", emitted)
	}
	if !strings.Contains(g.text(), "STOP") {
		t.Fatalf("total text must still record everything: %q", g.text())
	}
}

func TestStreamTextGateHoldsShellFenceOnlyWhenDeclared(t *testing.T) {
	var emitted []string
	// With a declared shell tool the whole pending buffer (including prose
	// before the fence) is held until the stream ends: it either becomes a
	// tool call or flushes as text.
	declared := newStreamTextGate(func(s string) error { emitted = append(emitted, s); return nil }, nil, "bash", false)
	if _, err := declared.push("text before\n```bash\necho hi"); err != nil {
		t.Fatal(err)
	}
	if len(emitted) != 0 {
		t.Fatalf("shell fence must hold everything, emitted %q", emitted)
	}
	if declared.pendingText() != "text before\n```bash\necho hi" {
		t.Fatalf("pending = %q", declared.pendingText())
	}

	emitted = nil
	undeclared := newStreamTextGate(func(s string) error { emitted = append(emitted, s); return nil }, nil, "", false)
	for _, d := range []string{"```bash\necho hi\n", "``` and more text to pass the holdback"} {
		if _, err := undeclared.push(d); err != nil {
			t.Fatal(err)
		}
	}
	if len(emitted) == 0 {
		t.Fatal("without a declared shell tool the bash block must stream")
	}
	if undeclared.text() != "```bash\necho hi\n``` and more text to pass the holdback" {
		t.Fatalf("text = %q", undeclared.text())
	}
}

func TestStreamTextGateFenceBoundarySplitsEmission(t *testing.T) {
	var emitted []string
	g := newStreamTextGate(func(s string) error { emitted = append(emitted, s); return nil }, nil, "", false)
	if _, err := g.push("Answer:\n```python\nprint(1)"); err != nil {
		t.Fatal(err)
	}
	if len(emitted) != 1 || emitted[0] != "Answer:\n" {
		t.Fatalf("emitted = %q, want prefix before the fence", emitted)
	}
	if g.pendingText() != "```python\nprint(1)" {
		t.Fatalf("pending = %q", g.pendingText())
	}
}

func TestStreamTextGateHoldAllAccumulatesOnly(t *testing.T) {
	var emitted []string
	g := newStreamTextGate(func(s string) error { emitted = append(emitted, s); return nil }, []string{"STOP"}, "bash", true)
	for _, d := range []string{"a", "STOP", "b"} {
		hit, err := g.push(d)
		if err != nil {
			t.Fatal(err)
		}
		if hit {
			t.Fatal("holdAll mode must not fire stops")
		}
	}
	if len(emitted) != 0 {
		t.Fatalf("holdAll must never emit: %q", emitted)
	}
	if g.text() != "aSTOPb" {
		t.Fatalf("text = %q", g.text())
	}
}

func TestStreamTextGateIngestFallback(t *testing.T) {
	var emitted []string
	g := newStreamTextGate(func(s string) error { emitted = append(emitted, s); return nil }, nil, "", false)
	g.ingestFallback("final answer")
	if g.text() != "final answer" || g.pendingText() != "final answer" {
		t.Fatalf("text=%q pending=%q", g.text(), g.pendingText())
	}
	if len(emitted) != 0 {
		t.Fatalf("fallback must not stream: %q", emitted)
	}
}

func TestStreamTextGateStopHoldCoversLongestStop(t *testing.T) {
	var emitted []string
	g := newStreamTextGate(func(s string) error { emitted = append(emitted, s); return nil }, []string{"averylongstopsequence"}, "", false)
	// Text shorter than the longest stop must stay fully held.
	if _, err := g.push("partial averylong"); err != nil {
		t.Fatal(err)
	}
	if len(emitted) != 0 {
		t.Fatalf("partial stop match leaked: %q", emitted)
	}
	if _, err := g.push("stopsequence done"); err != nil {
		t.Fatal(err)
	}
	if len(emitted) != 1 || emitted[0] != "partial " {
		t.Fatalf("emitted = %q, want [partial ]", emitted)
	}
}
