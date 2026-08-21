package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func TestParseAccountImport(t *testing.T) {
	entries, err := parseAccountImport("tokenA\ntokenB\n\n  tokenC  ")
	if err != nil || len(entries) != 3 || entries[0].RefreshToken != "tokenA" || entries[2].RefreshToken != "tokenC" {
		t.Fatalf("plain text entries=%#v err=%v", entries, err)
	}
	entries, err = parseAccountImport(`[{"refreshToken":"t1","clientId":"c1"},{"refreshToken":"t2"}]`)
	if err != nil || len(entries) != 2 || entries[0].ClientID != "c1" || entries[1].ClientID != "" {
		t.Fatalf("json array entries=%#v err=%v", entries, err)
	}
	entries, err = parseAccountImport(`{"tokens":[{"refreshToken":"t3"}]}`)
	if err != nil || len(entries) != 1 || entries[0].RefreshToken != "t3" {
		t.Fatalf("json object entries=%#v err=%v", entries, err)
	}
	if _, err := parseAccountImport("   "); err == nil {
		t.Fatal("empty input accepted")
	}
	if _, err := parseAccountImport("[{"); err == nil {
		t.Fatal("invalid json accepted")
	}
	if _, err := parseAccountImport(`{"other":1}`); err == nil {
		t.Fatal("object without tokens accepted")
	}
	big := make([]map[string]string, maxAccountImportBatch+1)
	for i := range big {
		big[i] = map[string]string{"refreshToken": "token"}
	}
	b, _ := json.Marshal(big)
	if _, err := parseAccountImport(string(b)); err == nil {
		t.Fatal("oversized batch accepted")
	}
}

func TestImportAccountsBatch(t *testing.T) {
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store}
	login := func(entry accountImportEntry) (auth.TokenSet, error) {
		switch entry.RefreshToken {
		case "good-a":
			return auth.TokenSet{AccessToken: "at-a", RefreshToken: "rt-a", ExpiresAt: time.Now().Add(time.Hour), Email: "a@example.com", HomeOID: "oid-a", TenantID: "tid-a", ClientID: entry.ClientID}, nil
		case "good-a-again":
			return auth.TokenSet{AccessToken: "at-a2", RefreshToken: "rt-a2", ExpiresAt: time.Now().Add(time.Hour), Email: "a@example.com", HomeOID: "oid-a", TenantID: "tid-a"}, nil
		case "bad":
			return auth.TokenSet{}, errors.New("invalid_grant")
		default:
			return auth.TokenSet{}, errors.New("unknown token")
		}
	}
	res := s.importAccountsBatch([]accountImportEntry{
		{RefreshToken: "good-a", ClientID: "cid-a"},
		{RefreshToken: "bad"},
		{RefreshToken: "good-a-again"},
	}, login)
	if res.Total != 3 || res.Imported != 1 || res.Updated != 1 || len(res.Failed) != 1 {
		t.Fatalf("res=%#v", res)
	}
	if res.Failed[0].Index != 1 || res.Failed[0].Token != "..." || !strings.Contains(res.Failed[0].Error, "invalid_grant") {
		t.Fatalf("failed entry=%#v", res.Failed)
	}
	acc, ok := s.tokens.Get("a@example.com")
	if !ok {
		t.Fatal("imported account missing from store")
	}
	if acc.ClientID != "cid-a" {
		t.Fatalf("per-account client id lost: %q", acc.ClientID)
	}
	if acc.RefreshToken != "rt-a2" {
		t.Fatalf("update did not refresh tokens: %q", acc.RefreshToken)
	}
}

func TestImportAccountsBatchWithPasswordEntries(t *testing.T) {
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store}
	login := func(entry accountImportEntry) (auth.TokenSet, error) {
		if entry.Email == "ok@example.com" && entry.Password == "secret" {
			return auth.TokenSet{AccessToken: "at-p", RefreshToken: "rt-p", ExpiresAt: time.Now().Add(time.Hour), Email: entry.Email, HomeOID: "oid-p", TenantID: "tid-p", ClientID: entry.ClientID}, nil
		}
		if entry.Email != "" {
			return auth.TokenSet{}, errors.New("AADSTS50126: invalid username or password")
		}
		return auth.TokenSet{}, errors.New("missing credentials")
	}
	res := s.importAccountsBatch([]accountImportEntry{
		{Email: "ok@example.com", Password: "secret", ClientID: "ropc-client"},
		{Email: "bad@example.com", Password: "wrong"},
	}, login)
	if res.Total != 2 || res.Imported != 1 || len(res.Failed) != 1 {
		t.Fatalf("res=%#v", res)
	}
	if res.Failed[0].Email != "ba***@example.com" || res.Failed[0].Token != "" || !strings.Contains(res.Failed[0].Error, "AADSTS50126") {
		t.Fatalf("failed entry=%#v", res.Failed)
	}
	acc, ok := s.tokens.Get("ok@example.com")
	if !ok || acc.ClientID != "ropc-client" || acc.RefreshToken != "rt-p" {
		t.Fatalf("account=%#v ok=%v", acc, ok)
	}
}

func TestExportAccountsReturnsCompleteLoginData(t *testing.T) {
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store}
	if _, err := s.tokens.Upsert(auth.TokenSet{
		AccessToken:  "header.eyJvaWQiOiJvaWQtYSIsInVuaXF1ZV9uYW1lIjoiYUBleGFtcGxlLmNvbSIsInRpZCI6InRpZC0xIn0.sig",
		RefreshToken: "refresh-a",
		ExpiresAt:    time.Now().Add(time.Hour),
		Email:        "a@example.com",
		DisplayName:  "Alice",
		HomeOID:      "oid-a",
		TenantID:     "tid-1",
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	s.exportAccounts(rr, httptest.NewRequest(http.MethodGet, "/api/accounts/export", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var res struct {
		Count    int                 `json:"count"`
		Accounts []exportAccountView `json:"accounts"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Count != 1 || len(res.Accounts) != 1 {
		t.Fatalf("export count = %d, want 1", res.Count)
	}
	acc := res.Accounts[0]
	if acc.RefreshToken != "refresh-a" || acc.AccessToken == "" || acc.Email != "a@example.com" || acc.OID != "oid-a" {
		t.Fatalf("export is missing complete login data: %+v", acc)
	}

	rr = httptest.NewRecorder()
	s.exportAccounts(rr, httptest.NewRequest(http.MethodGet, "/api/accounts/export?ids=missing", nil))
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Count != 0 {
		t.Fatalf("filtered export count = %d, want 0", res.Count)
	}
}

func TestAccountImportUIExposesControls(t *testing.T) {
	b := readIndexHTMLForTest(t)
	for _, marker := range []string{
		`id="accountImportInput"`,
		`id="accountImportBtn"`,
		`function accountImport()`,
		`function exportAccounts()`,
		`function viewAccountJSON(`,
		`/api/accounts/import`,
		`/api/accounts/export`,
	} {
		if !strings.Contains(b, marker) {
			t.Errorf("web UI is missing marker %q", marker)
		}
	}
}
