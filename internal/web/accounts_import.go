package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/auth"
)

const (
	maxAccountImportBatch = 100
	accountImportWorkers  = 5
	accountImportTimeout  = 25 * time.Second
)

// accountImportEntry is one account to import: either a refresh token or an
// email/password pair (ROPC login), plus an optional per-account OAuth client id.
type accountImportEntry struct {
	RefreshToken string `json:"refreshToken"`
	Email        string `json:"email,omitempty"`
	Password     string `json:"password,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
}

type accountImportFailure struct {
	Index int    `json:"index"`
	Token string `json:"token,omitempty"` // masked refresh token
	Email string `json:"email,omitempty"` // masked email for password entries
	Error string `json:"error"`
}

type accountImportResult struct {
	Total    int                    `json:"total"`
	Imported int                    `json:"imported"`
	Updated  int                    `json:"updated"`
	Failed   []accountImportFailure `json:"failed,omitempty"`
}

// importAccounts implements POST /api/accounts/import: batch-add M365
// accounts by redeeming refresh tokens. Each token is exchanged at the
// Microsoft token endpoint, so a batch import never needs an interactive
// browser flow per account.
func (s *Server) importAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	entries, err := parseAccountImport(string(raw))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	jsonOut(w, s.importAccountsBatch(entries, defaultAccountLogin))
}

// defaultAccountLogin redeems an entry at the Microsoft token endpoint:
// email/password pairs use the ROPC grant, everything else uses the refresh
// token grant.
func defaultAccountLogin(entry accountImportEntry) (auth.TokenSet, error) {
	if entry.Email != "" && entry.Password != "" {
		return auth.LoginWithPassword(entry.Email, entry.Password, entry.ClientID)
	}
	return auth.RefreshWithClient(entry.RefreshToken, entry.ClientID)
}

// parseAccountImport accepts a JSON array of {refreshToken, clientId} entries,
// a JSON object {"tokens":[...]}, or a plain-text blob with one refresh token
// per line.
func parseAccountImport(raw string) ([]accountImportEntry, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("no refresh tokens provided")
	}
	var entries []accountImportEntry
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
			return nil, fmt.Errorf("invalid JSON array: %v", err)
		}
	} else if strings.HasPrefix(trimmed, "{") {
		var obj struct {
			Tokens []accountImportEntry `json:"tokens"`
		}
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil || len(obj.Tokens) == 0 {
			return nil, fmt.Errorf("invalid JSON object: expected {tokens:[...]}")
		}
		entries = obj.Tokens
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				entries = append(entries, accountImportEntry{RefreshToken: line})
			}
		}
	}
	out := make([]accountImportEntry, 0, len(entries))
	for _, e := range entries {
		e.RefreshToken = strings.TrimSpace(e.RefreshToken)
		e.Email = strings.TrimSpace(e.Email)
		e.Password = strings.TrimSpace(e.Password)
		e.ClientID = strings.TrimSpace(e.ClientID)
		if e.RefreshToken == "" && (e.Email == "" || e.Password == "") {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no refresh tokens provided")
	}
	if len(out) > maxAccountImportBatch {
		return nil, fmt.Errorf("batch too large: %d entries (max %d)", len(out), maxAccountImportBatch)
	}
	return out, nil
}

func maskToken(t string) string {
	if len(t) <= 10 {
		return "..."
	}
	return t[:8] + "..."
}

func maskEmail(e string) string {
	at := strings.IndexByte(e, '@')
	if at <= 0 {
		return "..."
	}
	if at <= 2 {
		return e[:1] + "***" + e[at:]
	}
	return e[:2] + "***" + e[at:]
}

// importAccountsBatch redeems each entry through the injectable refresh
// function and persists successful exchanges, returning per-entry results.
func (s *Server) importAccountsBatch(entries []accountImportEntry, login func(entry accountImportEntry) (auth.TokenSet, error)) accountImportResult {
	res := accountImportResult{Total: len(entries)}
	type job struct {
		index int
		entry accountImportEntry
	}
	jobs := make(chan job)
	failures := make(chan accountImportFailure, len(entries))
	var mu sync.Mutex
	var imported, updated int
	var wg sync.WaitGroup
	for i := 0; i < accountImportWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				set, err := refreshWithTimeout(j.entry, login)
				if err != nil {
					failures <- entryFailure(j.index, j.entry, err.Error())
					continue
				}
				_, isNew, err := s.tokens.UpsertTracked(set)
				if err != nil {
					failures <- entryFailure(j.index, j.entry, "save account: "+err.Error())
					continue
				}
				mu.Lock()
				if isNew {
					imported++
				} else {
					updated++
				}
				mu.Unlock()
			}
		}()
	}
	for i, e := range entries {
		jobs <- job{index: i, entry: e}
	}
	close(jobs)
	wg.Wait()
	close(failures)
	for f := range failures {
		res.Failed = append(res.Failed, f)
	}
	sort.Slice(res.Failed, func(i, j int) bool { return res.Failed[i].Index < res.Failed[j].Index })
	res.Imported, res.Updated = imported, updated
	return res
}

func entryFailure(index int, entry accountImportEntry, msg string) accountImportFailure {
	f := accountImportFailure{Index: index, Error: msg}
	if entry.Email != "" {
		f.Email = maskEmail(entry.Email)
	} else {
		f.Token = maskToken(entry.RefreshToken)
	}
	return f
}

func refreshWithTimeout(entry accountImportEntry, login func(accountImportEntry) (auth.TokenSet, error)) (auth.TokenSet, error) {
	type pair struct {
		set auth.TokenSet
		err error
	}
	done := make(chan pair, 1)
	go func() {
		set, err := login(entry)
		done <- pair{set, err}
	}()
	select {
	case p := <-done:
		return p.set, p.err
	case <-time.After(accountImportTimeout):
		return auth.TokenSet{}, fmt.Errorf("timeout exchanging refresh token")
	}
}
