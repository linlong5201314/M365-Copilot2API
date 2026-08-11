package main

import "testing"

func TestResolveListenAddress(t *testing.T) {
	tests := []struct {
		name       string
		port       string
		configured string
		want       string
	}{
		{
			name: "local default remains loopback only",
			want: "127.0.0.1:4141",
		},
		{
			name:       "explicit M365 listen address is preserved",
			configured: "0.0.0.0:8080",
			want:       "0.0.0.0:8080",
		},
		{
			name: "Railway port binds all interfaces",
			port: "3000",
			want: "0.0.0.0:3000",
		},
		{
			name:       "platform port overrides the image default",
			port:       "3000",
			configured: "0.0.0.0:4141",
			want:       "0.0.0.0:3000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveListenAddress(tt.port, tt.configured); got != tt.want {
				t.Fatalf("resolveListenAddress(%q, %q) = %q, want %q", tt.port, tt.configured, got, tt.want)
			}
		})
	}
}
