package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoverPanicsPreservesFlusher(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: ok\n\n")
		flusher.Flush()
	})
	h := recoverPanics(requestID(httpTrace(securityHeaders(inner))))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: ok") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRoutesStreamDoesNotRejectMissingFlusher(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_DEBUG_LOG", dir+"/debug.jsonl")
	t.Setenv("M365_ADMIN_PASSWORD", "test-admin-password")
	t.Setenv("M365_API_KEYS", dir+"/api-keys.json")
	t.Setenv("M365_CONFIG", dir+"/accounts.json")
	t.Setenv("M365_SESSION_CACHE", dir+"/sessions.json")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec, raw, err := s.apiKeys.create("stream-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = rec
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusInternalServerError && strings.Contains(string(body), "stream unsupported") {
		t.Fatalf("middleware stripped http.Flusher: status=%d body=%s", resp.StatusCode, body)
	}
}
