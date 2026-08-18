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

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "legacy API health", path: "/api/health", wantStatus: http.StatusOK, wantBody: "json"},
		{name: "Railway probe", path: "/healthz", wantStatus: http.StatusOK, wantBody: "ok\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			resp := httptest.NewRecorder()

			s.Routes().ServeHTTP(resp, req)

			if resp.Code != tt.wantStatus {
				t.Fatalf("health status = %d, want %d", resp.Code, tt.wantStatus)
			}
			if tt.wantBody == "ok\n" {
				if got := resp.Body.String(); got != tt.wantBody {
					t.Fatalf("health body = %q, want %q", got, tt.wantBody)
				}
				return
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
		})
	}
}

func TestHealthProbeDoesNotBypassAdminAuthentication(t *testing.T) {
	s := &Server{
		tokens:        &auth.Store{},
		adminPassword: "test-admin-password",
		adminSessions: map[string]time.Time{},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	resp := httptest.NewRecorder()

	s.Routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("protected route status = %d, want %d", resp.Code, http.StatusUnauthorized)
	}
}
