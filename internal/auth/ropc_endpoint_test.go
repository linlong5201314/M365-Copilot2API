package auth

import "testing"

func TestROPCTokenEndpoint(t *testing.T) {
	cases := []struct {
		name      string
		authority string
		want      string
	}{
		{"default common maps to organizations", "https://login.microsoftonline.com/common", "https://login.microsoftonline.com/organizations/oauth2/v2.0/token"},
		{"organizations stays", "https://login.microsoftonline.com/organizations", "https://login.microsoftonline.com/organizations/oauth2/v2.0/token"},
		{"bare origin appends organizations", "https://login.microsoftonline.com", "https://login.microsoftonline.com/organizations/oauth2/v2.0/token"},
		{"consumers maps to organizations", "https://login.microsoftonline.com/consumers", "https://login.microsoftonline.com/organizations/oauth2/v2.0/token"},
		{"trailing slash handled", "https://login.microsoftonline.com/common/", "https://login.microsoftonline.com/organizations/oauth2/v2.0/token"},
		{"tenant specific respected", "https://login.microsoftonline.com/contoso.onmicrosoft.com", "https://login.microsoftonline.com/contoso.onmicrosoft.com/oauth2/v2.0/token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("M365_AUTHORITY", tc.authority)
			t.Setenv("M365_TOKEN_ENDPOINT", "")
			if got := ropcTokenEndpoint(); got != tc.want {
				t.Fatalf("ropcTokenEndpoint(%q) = %q, want %q", tc.authority, got, tc.want)
			}
		})
	}
	t.Run("explicit token endpoint wins", func(t *testing.T) {
		t.Setenv("M365_AUTHORITY", "https://login.microsoftonline.com/common")
		t.Setenv("M365_TOKEN_ENDPOINT", "https://proxy.example/token")
		if got := ropcTokenEndpoint(); got != "https://proxy.example/token" {
			t.Fatalf("ropcTokenEndpoint with M365_TOKEN_ENDPOINT = %q, want proxy", got)
		}
	})
}
