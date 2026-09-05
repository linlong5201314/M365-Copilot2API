package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"m365-copilot2api/internal/auth"
)

func logOAuthError(stage string, err error) {
	var oauthErr *auth.OAuthError
	if errors.As(err, &oauthErr) {
		log.Printf("oauth_error stage=%s error=%q aadsts=%q http_status=%d correlation_id=%q trace_id=%q", stage, oauthErr.Code, oauthErr.AADSTS, oauthErr.HTTPStatus, oauthErr.CorrelationID, oauthErr.TraceID)
		return
	}
	log.Printf("oauth_error stage=%s error=%q", stage, "request_failed")
}

// upstreamError keeps transport details, including URLs and credentials, out
// of client-visible responses while retaining a server-side diagnostic.
func upstreamError(err error) string {
	if err == nil {
		return "upstream request failed"
	}
	log.Printf("upstream request failed: %v", err)
	return "upstream request failed"
}

// upstreamStatus maps a failed upstream call to the client-visible HTTP status:
// rate limits stay 429 (with Retry-After when known), auth failures become 401,
// disengaged turns rest at 503, everything else is 502. Unknown upstream
// failures must never leak internals.
func upstreamStatus(err error) int {
	if IsRateLimited(err) {
		return http.StatusTooManyRequests
	}
	if IsAuthFailure(err) {
		return http.StatusUnauthorized
	}
	if IsDisengaged(err) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

// writeUpstreamError renders a failed upstream call as an HTTP response,
// surfacing the Retry-After hint for rate limits so clients can back off.
func writeUpstreamError(w http.ResponseWriter, err error) {
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	http.Error(w, upstreamError(err), upstreamStatus(err))
}

// upstreamErrorCode maps a failed upstream call to a coarse client-safe code
// for SSE error chunks. Consumers use it for backoff decisions, so distinct
// failure classes must not all collapse into "rate_limit".
func upstreamErrorCode(err error) string {
	if err == nil {
		return "upstream_error"
	}
	if IsRateLimited(err) {
		return "rate_limit"
	}
	if IsAuthFailure(err) {
		return "upstream_auth"
	}
	if IsDisengaged(err) {
		return "content_filter"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "upstream_error"
}
