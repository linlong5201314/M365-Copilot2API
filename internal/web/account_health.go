package web

import (
	"errors"
	"fmt"
	"m365-copilot2api/internal/chathub"
	"strings"
	"sync"
	"time"
)

// UpstreamHTTPError carries the HTTP status of a failed upstream request so
// callers can distinguish rate limiting (429), authorization issues (401/403)
// and transient server errors (5xx) from one another.
type UpstreamHTTPError struct {
	Status     int
	RetryAfter int
	Body       string
}

func (e *UpstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream http %d", e.Status)
}

// IsRateLimited reports whether err represents an upstream 429 or an
// indistinguishable throttling signal (rate limit, too many requests,
// throttled). Structured ChatHub throttle errors take precedence over
// free-form message matching.
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var throttleErr *chathub.ThrottleError
	if errors.As(err, &throttleErr) {
		return true
	}
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == 429 || httpErr.Status == 503
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "throttl")
}

// disengagedCooldown bounds how long an account rests after ChatHub's
// Disengaged gate fired. The state self-heals in roughly 15 minutes and
// hammering it again only extends the suppression, so rest longer than the
// 2-minute rate-limit window.
const disengagedCooldown = 15 * time.Minute

// IsDisengaged reports whether err is ChatHub's Disengaged gate (empty or
// filler-only output after upstream suppression).
func IsDisengaged(err error) bool {
	if err == nil {
		return false
	}
	var disengagedErr *chathub.DisengagedError
	return errors.As(err, &disengagedErr)
}

// IsAuthFailure reports whether err represents an upstream 401/403, meaning
// the account itself is unusable until re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == 401 || httpErr.Status == 403
	}
	return false
}

// RetryAfterSeconds returns the upstream Retry-After hint for a rate-limited
// error, or 0 when absent. The web layer surfaces this to clients so they can
// back off instead of hammering a throttled pool.
func RetryAfterSeconds(err error) int {
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.RetryAfter
	}
	return 0
}

// authFailTTL bounds how long an auth failure keeps an account out of
// rotation. Without it, a transient AAD outage (spurious 401/403) would pin
// every account as unusable until process restart.
const authFailTTL = 10 * time.Minute

// accountHealth tracks per-account failure state: rate-limited accounts are
// cooled down and skipped by the round-robin until the window expires, and
// auth-failed accounts are pinned as unusable until the TTL lapses.
type accountHealth struct {
	mu       sync.Mutex
	cooldown map[string]time.Time
	authFail map[string]time.Time
}

func newAccountHealth() *accountHealth {
	return &accountHealth{cooldown: map[string]time.Time{}, authFail: map[string]time.Time{}}
}

// MarkFailure records the outcome of a request for one account.
// rateLimited cools the account down for window; authFailed pins it for
// authFailTTL so transient identity-platform failures can self-heal.
func (h *accountHealth) MarkFailure(accountID string, err error, window time.Duration) {
	if window <= 0 {
		window = 60 * time.Second
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if IsAuthFailure(err) {
		h.authFail[accountID] = time.Now().Add(authFailTTL)
		delete(h.cooldown, accountID)
		return
	}
	if IsDisengaged(err) {
		delete(h.authFail, accountID)
		h.cooldown[accountID] = time.Now().Add(disengagedCooldown)
		return
	}
	if IsRateLimited(err) {
		delete(h.authFail, accountID)
		h.cooldown[accountID] = time.Now().Add(window)
	}
}

// MarkSuccess clears any failure state after a healthy response.
func (h *accountHealth) MarkSuccess(accountID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.cooldown, accountID)
	delete(h.authFail, accountID)
}

// Available reports whether the account may be used right now.
func (h *accountHealth) Available(accountID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if until, ok := h.authFail[accountID]; ok {
		if time.Now().Before(until) {
			return false
		}
		delete(h.authFail, accountID)
	}
	if until, ok := h.cooldown[accountID]; ok && time.Now().Before(until) {
		return false
	}
	return true
}

// Snapshot returns a copy of the current health state for the admin UI.
func (h *accountHealth) Snapshot() map[string]map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	out := make(map[string]map[string]any, len(h.cooldown)+len(h.authFail))
	for id, until := range h.cooldown {
		out[id] = map[string]any{"available": now.After(until), "cooldownUntil": until}
	}
	for id, until := range h.authFail {
		if now.After(until) {
			// Expired auth failure: treat as healthy again.
			delete(h.authFail, id)
			continue
		}
		if _, ok := out[id]; !ok {
			out[id] = map[string]any{}
		}
		out[id]["authFailed"] = true
		out[id]["authFailUntil"] = until
	}
	return out
}