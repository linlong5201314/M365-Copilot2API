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
	}, login)
	if res.Total != 2 || res.Imported != 1 || res.Updated != 0 || len(res.Failed) != 1 {
		t.Fatalf("first batch res=%#v", res)
	}
	if res.Failed[0].Index != 1 || res.Failed[0].Token != "..." || !strings.Contains(res.Failed[0].Error, "invalid_grant") {
		t.Fatalf("failed entry=%#v", res.Failed)
	}
	// Re-importing the same account in a separate batch is a deterministic
	// update (avoids the concurrent-worker ordering race from a single batch).
	res = s.importAccountsBatch([]accountImportEntry{{RefreshToken: "good-a-again"}}, login)
	if res.Total != 1 || res.Imported != 0 || res.Updated != 1 || len(res.Failed) != 0 {
		t.Fatalf("second batch res=%#v", res)
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

func TestParseAccountImportAcceptsExportJSON(t *testing.T) {
	raw := `{"exportedAt":"2026-08-21T00:00:00Z","count":2,"accounts":[{"id":"oid-a","email":"a@example.com","displayName":"Alice","status":"online","oid":"oid-a","tid":"tid-1","clientId":"cid-a","accessToken":"at-a","refreshToken":"rt-a"},{"id":"oid-b","email":"b@example.com","oid":"oid-b","tid":"tid-2","clientId":"cid-b","accessToken":"at-b","refreshToken":"rt-b"}]}`
	entries, err := parseAccountImport(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries=%d, want 2", len(entries))
	}
	e := entries[0]
	if e.RefreshToken != "rt-a" || e.OID != "oid-a" || e.TID != "tid-1" || e.ClientID != "cid-a" || e.AccessToken != "at-a" || e.Email != "a@example.com" || e.DisplayName != "Alice" {
		t.Fatalf("entry=%#v", e)
	}
}

func TestDefaultAccountLoginDirectImport(t *testing.T) {
	set, err := defaultAccountLogin(accountImportEntry{
		RefreshToken: "rt", AccessToken: "at", Email: "a@example.com", DisplayName: "Alice",
		OID: "oid-a", TID: "tid-1", ClientID: "cid-a", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if set.RefreshToken != "rt" || set.AccessToken != "at" || set.HomeOID != "oid-a" || set.TenantID != "tid-1" || set.ClientID != "cid-a" || set.Email != "a@example.com" {
		t.Fatalf("set=%#v", set)
	}
}

func TestExportThenReimportRoundTrip(t *testing.T) {
	src, err := auth.OpenStore(filepath.Join(t.TempDir(), "src.json"))
	if err != nil {
		t.Fatal(err)
	}
	srcSrv := &Server{tokens: src}
	if _, err := src.Upsert(auth.TokenSet{
		AccessToken: "at-a", RefreshToken: "rt-a", Email: "a@example.com",
		DisplayName: "Alice", HomeOID: "oid-a", TenantID: "tid-1", ClientID: "cid-a",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srcSrv.exportAccounts(rr, httptest.NewRequest(http.MethodGet, "/api/accounts/export", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rr.Code, rr.Body.String())
	}

	entries, err := parseAccountImport(rr.Body.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(entries))
	}

	dst, err := auth.OpenStore(filepath.Join(t.TempDir(), "dst.json"))
	if err != nil {
		t.Fatal(err)
	}
	dstSrv := &Server{tokens: dst}
	res := dstSrv.importAccountsBatch(entries, defaultAccountLogin)
	if res.Total != 1 || res.Imported != 1 || len(res.Failed) != 0 {
		t.Fatalf("import res=%#v", res)
	}
	acc, ok := dst.Get("oid-a")
	if !ok {
		t.Fatal("round-trip account missing")
	}
	if acc.Email != "a@example.com" || acc.RefreshToken != "rt-a" || acc.ClientID != "cid-a" || acc.TID != "tid-1" || acc.OID != "oid-a" {
		t.Fatalf("account=%#v", acc)
	}
}
