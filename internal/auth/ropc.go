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
	set, err := requestToken(form)
	set.ClientID = clientID
	return set, err
}
