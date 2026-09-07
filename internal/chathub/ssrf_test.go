package chathub

import (
	"net"
	"testing"
)

func TestValidateRemoteDownloadURLRejectsLoopback(t *testing.T) {
	if err := validateRemoteDownloadURL("http://example.com/a.png"); err == nil {
		t.Fatal("http must be rejected")
	}
	if err := validateRemoteDownloadURL("https://127.0.0.1/a.png"); err == nil {
		t.Fatal("loopback must be rejected")
	}
	if err := validateRemoteDownloadURL("https://10.0.0.1/a.png"); err == nil {
		t.Fatal("private IPv4 must be rejected")
	}
}

func TestIPUnsafeCoversCGNATAndMetadata(t *testing.T) {
	if !ipUnsafe(net.ParseIP("100.64.1.1")) {
		t.Fatal("CGNAT should be unsafe")
	}
	if !ipUnsafe(net.ParseIP("169.254.169.254")) {
		t.Fatal("metadata should be unsafe")
	}
	if ipUnsafe(net.ParseIP("1.1.1.1")) {
		t.Fatal("public IP marked unsafe")
	}
}
