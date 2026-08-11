package web

import (
	"encoding/json"
	"m365-copilot2api/internal/auth"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthEndpointDoesNotRequireAdminSession(t *testing.T) {
	s := &Server{
		tokens:        &auth.Store{},
		adminPassword: "test-admin-password",
		adminSessions: map[string]time.Time{},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	resp := httptest.NewRecorder()

	s.Routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", resp.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("health status body = %q, want %q", body.Status, "ok")
	}
}
