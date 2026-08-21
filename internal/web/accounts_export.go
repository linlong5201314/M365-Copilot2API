package web

import (
	"net/http"
	"strings"
	"time"
)

// exportAccountView is the full login record of one account, including the
// refresh token, so exports can be re-imported or reused elsewhere.
type exportAccountView struct {
	ID           string    `json:"id"`
	Email        string    `json:"email,omitempty"`
	DisplayName  string    `json:"displayName,omitempty"`
	Status       string    `json:"status"`
	OID          string    `json:"oid,omitempty"`
	TID          string    `json:"tid,omitempty"`
	ClientID     string    `json:"clientId,omitempty"`
	AccessToken  string    `json:"accessToken,omitempty"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
}

// exportAccounts implements GET /api/accounts/export: return the complete
// login information (JSON) of all or selected accounts for external use. The
// fields are compatible with POST /api/accounts/import for round-trip
// migration between instances.
func (s *Server) exportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	wanted := map[string]bool{}
	for _, part := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if p := strings.TrimSpace(part); p != "" {
			wanted[p] = true
		}
	}
	list := s.tokens.List()
	out := make([]exportAccountView, 0, len(list))
	for _, a := range list {
		if len(wanted) > 0 && !wanted[a.ID] && !wanted[a.OID] && !wanted[a.Email] {
			continue
		}
		out = append(out, exportAccountView{
			ID: a.ID, Email: a.Email, DisplayName: a.DisplayName, Status: a.Status,
			OID: a.OID, TID: a.TID, ClientID: a.ClientID,
			AccessToken: a.AccessToken, RefreshToken: a.RefreshToken,
			ExpiresAt: a.ExpiresAt, UpdatedAt: a.UpdatedAt,
		})
	}
	jsonOut(w, map[string]any{
		"exportedAt": time.Now().UTC(),
		"count":      len(out),
		"accounts":   out,
	})
}
