package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readIndexHTMLForTest(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web/index.html: %v", err)
	}
	return string(b)
}

func TestAccountAuthorizationOffersCopyableLink(t *testing.T) {
	html := readIndexHTMLForTest(t)
	for _, marker := range []string{
		`id="authLinkInput"`,
		`id="copyAuthLinkBtn"`,
		`onclick="copyAuthLink()"`,
		`function copyAuthLink()`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("web UI is missing authorization-link marker %q", marker)
		}
	}
}

func TestStartPKCEStoresAuthorizationLinkForCopy(t *testing.T) {
	html := readIndexHTMLForTest(t)
	if !strings.Contains(html, `$('authLinkInput').value=d.url`) {
		t.Error("startPKCE must expose the returned authorization URL")
	}
	if !strings.Contains(html, `navigator.clipboard.writeText(link)`) {
		t.Error("copyAuthLink must write the authorization URL to the clipboard")
	}
}
