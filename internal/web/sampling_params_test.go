package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

func intPtr(v int) *int { return &v }

func floatPtr(v float64) *float64 { return &v }

func TestNormalizeSamplingParams(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(b *oaiReq)
		wantErr bool
		check   func(*testing.T, *samplingParams)
	}{
		{
			name:   "empty defaults",
			mutate: func(b *oaiReq) {},
			check: func(t *testing.T, p *samplingParams) {
				if p.maxTokens != 0 || p.includeUsage || len(p.stops) != 0 || p.stopRunes != 0 {
					t.Fatalf("unexpected defaults: %+v", p)
				}
			},
		},
		{name: "n>1 rejected", mutate: func(b *oaiReq) { b.N = intPtr(2) }, wantErr: true},
		{name: "n=1 allowed", mutate: func(b *oaiReq) { b.N = intPtr(1) }},
		{name: "temperature too high", mutate: func(b *oaiReq) { b.Temperature = floatPtr(2.5) }, wantErr: true},
		{name: "temperature negative", mutate: func(b *oaiReq) { b.Temperature = floatPtr(-0.1) }, wantErr: true},
		{name: "top_p out of range", mutate: func(b *oaiReq) { b.TopP = floatPtr(1.5) }, wantErr: true},
		{name: "presence_penalty out of range", mutate: func(b *oaiReq) { b.PresencePenalty = floatPtr(-3) }, wantErr: true},
		{name: "frequency_penalty out of range", mutate: func(b *oaiReq) { b.FreqPenalty = floatPtr(3) }, wantErr: true},
		{
			name:   "completion tokens override max tokens",
			mutate: func(b *oaiReq) { b.MaxTokens = intPtr(10); b.CompletionTokens = intPtr(20) },
			check: func(t *testing.T, p *samplingParams) {
				if p.maxTokens != 20 {
					t.Fatalf("maxTokens = %d, want 20", p.maxTokens)
				}
			},
		},
		{
			name:   "max_tokens only",
			mutate: func(b *oaiReq) { b.MaxTokens = intPtr(10) },
			check: func(t *testing.T, p *samplingParams) {
				if p.maxTokens != 10 {
					t.Fatalf("maxTokens = %d, want 10", p.maxTokens)
				}
			},
		},
		{
			name:   "string stop",
			mutate: func(b *oaiReq) { b.Stop = "END" },
			check: func(t *testing.T, p *samplingParams) {
				if len(p.stops) != 1 || p.stops[0] != "END" || p.stopRunes != 3 {
					t.Fatalf("unexpected stops: %+v", p)
				}
			},
		},
		{
			name:   "array stop",
			mutate: func(b *oaiReq) { b.Stop = []any{"a", "STOP"} },
			check: func(t *testing.T, p *samplingParams) {
				if len(p.stops) != 2 || p.stopRunes != 4 {
					t.Fatalf("unexpected stops: %+v", p)
				}
			},
		},
		{name: "non-string stop rejected", mutate: func(b *oaiReq) { b.Stop = []any{"a", 3} }, wantErr: true},
		{name: "invalid stop type rejected", mutate: func(b *oaiReq) { b.Stop = 42 }, wantErr: true},
		{
			name: "five stops rejected",
			mutate: func(b *oaiReq) {
				b.Stop = []any{"1", "2", "3", "4", "5"}
			},
			wantErr: true,
		},
		{
			name:   "include usage",
			mutate: func(b *oaiReq) { b.StreamOptions = &streamOptions{IncludeUsage: true} },
			check: func(t *testing.T, p *samplingParams) {
				if !p.includeUsage {
					t.Fatal("includeUsage not captured")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &oaiReq{}
			tc.mutate(b)
			p, err := normalizeSamplingParams(b)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, p)
			}
		})
	}
}

func TestFindStopSequence(t *testing.T) {
	if idx, hit := findStopSequence("hello WORLD tail", []string{"WORLD"}); !hit || idx != 6 {
		t.Fatalf("idx=%d hit=%v, want 6 true", idx, hit)
	}
	if _, hit := findStopSequence("abc", []string{"x", "y"}); hit {
		t.Fatal("no stop should match")
	}
	// Earliest occurrence wins regardless of stops order.
	if idx, hit := findStopSequence("aZZbYYc", []string{"YY", "ZZ"}); !hit || idx != 1 {
		t.Fatalf("idx=%d hit=%v, want 1 true", idx, hit)
	}
	if _, hit := findStopSequence("abc", nil); hit {
		t.Fatal("empty stops must not match")
	}
}

func TestTruncateToTokens(t *testing.T) {
	text := strings.Repeat("hello world ", 500)
	cut, truncated := truncateToTokens(text, 10)
	if !truncated || len(cut) >= len(text) {
		t.Fatalf("truncated=%v cutLen=%d", truncated, len(cut))
	}
	if n := EstimateTokens(cut); n > 10 {
		t.Fatalf("cut produced %d tokens, want <= 10", n)
	}
	full, truncated := truncateToTokens("short text", 100)
	if truncated || full != "short text" {
		t.Fatalf("short text must pass through: truncated=%v", truncated)
	}
	same, truncated := truncateToTokens("abc", 0)
	if truncated || same != "abc" {
		t.Fatal("max<=0 must be a no-op")
	}
}

func TestEstimateTokensUsesTiktoken(t *testing.T) {
	if _, err := getGPTTokenizer(); err != nil {
		t.Skipf("tokenizer unavailable: %v", err)
	}
	// "hello world" encodes to 2 tokens in O200kBase.
	if got := EstimateTokens("hello world"); got != 2 {
		t.Fatalf("EstimateTokens = %d, want 2", got)
	}
	if got := EstimateTokens(""); got != 0 {
		t.Fatalf("EstimateTokens(empty) = %d, want 0", got)
	}
}

func TestEnvDurationMinutes(t *testing.T) {
	def := 2 * time.Hour
	cases := map[string]time.Duration{
		"":      def,
		"120":   120 * time.Minute,
		"2h":    2 * time.Hour,
		"90m":   90 * time.Minute,
		"bogus": def,
		"-5":    def,
		"0":     def,
		"-3m":   def,
	}
	for in, want := range cases {
		if got := envDurationMinutes(in, def); got != want {
			t.Fatalf("envDurationMinutes(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestUpstreamErrorCodeMapping(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "upstream_error"},
		{&chathub.ThrottleError{Value: "Throttled"}, "rate_limit"},
		{&UpstreamHTTPError{Status: 429}, "rate_limit"},
		{&UpstreamHTTPError{Status: 401}, "upstream_auth"},
		{&chathub.DisengagedError{Message: "x"}, "content_filter"},
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "cancelled"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "timeout"},
		{fmt.Errorf("boom"), "upstream_error"},
	}
	for _, tc := range cases {
		if got := upstreamErrorCode(tc.err); got != tc.want {
			t.Fatalf("upstreamErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestAccountHealthAuthFailTTLRecovery(t *testing.T) {
	h := newAccountHealth()
	h.MarkFailure("a", &UpstreamHTTPError{Status: 401}, time.Minute)
	if h.Available("a") {
		t.Fatal("auth-failed account must be unavailable")
	}
	// Force-expire the TTL; the account must self-heal without MarkSuccess.
	h.mu.Lock()
	h.authFail["a"] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if !h.Available("a") {
		t.Fatal("expired auth failure must self-heal")
	}
}

func TestAccountHealthDisengagedCooldown(t *testing.T) {
	h := newAccountHealth()
	h.MarkFailure("a", &chathub.DisengagedError{Message: "no content produced"}, time.Minute)
	if h.Available("a") {
		t.Fatal("disengaged account must be unavailable")
	}
	h.mu.Lock()
	until := h.cooldown["a"]
	h.mu.Unlock()
	if time.Until(until) < disengagedCooldown-time.Minute {
		t.Fatalf("disengaged cooldown too short: %v", time.Until(until))
	}
	// A success clears it immediately.
	h.MarkSuccess("a")
	if !h.Available("a") {
		t.Fatal("MarkSuccess must clear disengaged cooldown")
	}
}
