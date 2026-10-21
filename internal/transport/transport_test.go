package transport

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveServerURLs(t *testing.T) {
	tests := []struct {
		name            string
		raw             string
		allowInsecure   bool
		wantAPI         string
		wantWS          string
		wantErr         error
		wantErrContains string
	}{
		// --- TLS is the default ---
		{name: "scheme-less host defaults to TLS", raw: "media.example.com", wantAPI: "https://media.example.com", wantWS: "wss://media.example.com"},
		{name: "scheme-less host:port defaults to TLS", raw: "media.example.com:8443", wantAPI: "https://media.example.com:8443", wantWS: "wss://media.example.com:8443"},
		{name: "https preserved", raw: "https://media.example.com", wantAPI: "https://media.example.com", wantWS: "wss://media.example.com"},
		{name: "wss maps to https for API", raw: "wss://media.example.com", wantAPI: "https://media.example.com", wantWS: "wss://media.example.com"},
		{name: "trailing slash trimmed", raw: "https://media.example.com/", wantAPI: "https://media.example.com", wantWS: "wss://media.example.com"},
		{name: "path preserved", raw: "https://media.example.com/proxy", wantAPI: "https://media.example.com/proxy", wantWS: "wss://media.example.com/proxy"},
		{name: "IPv6 loopback with port", raw: "http://[::1]:8082", wantAPI: "http://[::1]:8082", wantWS: "ws://[::1]:8082"},

		// --- plaintext refused for non-loopback without opt-in ---
		{name: "http hostname refused", raw: "http://media.example.com", wantErr: ErrPlaintextTransport},
		{name: "http LAN IP refused", raw: "http://192.168.1.10:8082", wantErr: ErrPlaintextTransport},
		{name: "ws hostname refused", raw: "ws://media.example.com", wantErr: ErrPlaintextTransport},
		{name: "http allowed non-loopback with insecure opt-in", raw: "http://media.example.com", allowInsecure: true, wantAPI: "http://media.example.com", wantWS: "ws://media.example.com"},
		{name: "ws allowed non-loopback with insecure opt-in", raw: "ws://192.168.1.10:8082", allowInsecure: true, wantAPI: "http://192.168.1.10:8082", wantWS: "ws://192.168.1.10:8082"},

		// --- documented loopback dev exemption ---
		{name: "http localhost exempt", raw: "http://localhost:8082", wantAPI: "http://localhost:8082", wantWS: "ws://localhost:8082"},
		{name: "http 127.0.0.1 exempt", raw: "http://127.0.0.1:8082", wantAPI: "http://127.0.0.1:8082", wantWS: "ws://127.0.0.1:8082"},
		{name: "http 127.anything exempt", raw: "http://127.5.6.7:8082", wantAPI: "http://127.5.6.7:8082", wantWS: "ws://127.5.6.7:8082"},
		{name: "ws localhost exempt", raw: "ws://localhost:8082", wantAPI: "http://localhost:8082", wantWS: "ws://localhost:8082"},
		{name: "localhost with trailing dot exempt", raw: "http://localhost.:8082", wantAPI: "http://localhost.:8082", wantWS: "ws://localhost.:8082"},

		// --- malformed input ---
		{name: "empty refused", raw: "", wantErrContains: "server URL is empty"},
		{name: "unsupported scheme refused", raw: "ftp://media.example.com", wantErrContains: "unsupported server URL scheme"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiURL, wsURL, err := ResolveServerURLs(tt.raw, tt.allowInsecure)
			if tt.wantErr != nil || tt.wantErrContains != "" {
				if err == nil {
					t.Fatalf("ResolveServerURLs(%q) = (%q, %q, nil), want error", tt.raw, apiURL, wsURL)
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Fatalf("ResolveServerURLs(%q) error = %v, want %v", tt.raw, err, tt.wantErr)
				}
				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("ResolveServerURLs(%q) error = %v, want substring %q", tt.raw, err, tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveServerURLs(%q) unexpected error: %v", tt.raw, err)
			}
			if apiURL != tt.wantAPI || wsURL != tt.wantWS {
				t.Fatalf("ResolveServerURLs(%q) = (%q, %q), want (%q, %q)", tt.raw, apiURL, wsURL, tt.wantAPI, tt.wantWS)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	loopback := []string{"localhost", "LOCALHOST", "localhost.", "127.0.0.1", "127.0.0.7", "::1"}
	for _, h := range loopback {
		if !IsLoopbackHost(h) {
			t.Errorf("IsLoopbackHost(%q) = false, want true", h)
		}
	}
	nonLoopback := []string{"example.com", "192.168.1.1", "10.0.0.1", "::2", "localhost.evil.com", "", "127.example.com"}
	for _, h := range nonLoopback {
		if IsLoopbackHost(h) {
			t.Errorf("IsLoopbackHost(%q) = true, want false", h)
		}
	}
}
