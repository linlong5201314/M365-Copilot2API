package auth

import (
	"net/url"
	"os"
	"strings"
)

// ROPCClientID returns the OAuth client used for username/password (ROPC)
// logins. First-party Copilot clients never allow ROPC, so password login
// requires the operator to register their own Azure app with "Allow public
// client flows" enabled and pass its client id here.
func ROPCClientID() string {
	if v := os.Getenv("M365_ROPC_CLIENT_ID"); v != "" {
		return v
	}
	if v := os.Getenv("M365_PASSWORD_CLIENT_ID"); v != "" {
		return v
	}
	return DeviceClientID()
}

// ropcTokenEndpoint resolves the token endpoint for the password (ROPC)
// grant. Microsoft rejects password grants on the /common and /consumers
// authorities (AADSTS9001023), so those resolve to /organizations; a
// tenant-specific authority is respected as-is, and an explicit
// M365_TOKEN_ENDPOINT always wins.
func ropcTokenEndpoint() string {
	if endpoint := strings.TrimSpace(os.Getenv("M365_TOKEN_ENDPOINT")); endpoint != "" {
		return endpoint
	}
	authority := strings.TrimRight(Authority(), "/")
	u, err := url.Parse(authority)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return authority + "/oauth2/v2.0/token"
	}
	switch strings.Trim(u.Path, "/") {
	case "", "common", "consumers":
		return u.Scheme + "://" + u.Host + "/organizations/oauth2/v2.0/token"
	case "organizations":
		return authority + "/oauth2/v2.0/token"
	default:
		// Tenant-specific authority (tenant id or verified domain): password
		// grants are accepted there, so keep the operator's choice.
		return authority + "/oauth2/v2.0/token"
	}
}

// LoginWithPassword redeems an email/password pair through the OAuth ROPC
// (resource owner password credentials) grant. Accounts with MFA or tenant
// security defaults are rejected by Microsoft; the returned error carries the
// AADSTS code so callers can report it per account.
func LoginWithPassword(email, password, clientID string) (TokenSet, error) {
	if strings.TrimSpace(clientID) == "" {
		clientID = ROPCClientID()
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "password")
	form.Set("username", email)
	form.Set("password", password)
	form.Set("scope", Scope())
	set, err := requestTokenAt(form, ropcTokenEndpoint())
	set.ClientID = clientID
	return set, err
}
