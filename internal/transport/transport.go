// Package transport centralizes the agent's outbound connection policy.
//
// TLS is the default for every endpoint the agent talks to: scheme-less
// server addresses resolve to https:// (API) and wss:// (websocket), and
// plain http:// / ws:// endpoints are refused outright unless the host is
// loopback or the user explicitly passed --insecure.
//
// Loopback judgment call (documented): plaintext to localhost / 127.0.0.0/8
// / ::1 is permitted without --insecure so local development against a
// plain-HTTP API server keeps working. Credential traffic (pairing codes,
// API keys, JWTs) to any other host always requires TLS or --insecure.
// --insecure only permits plaintext transport; certificate verification is
// never disabled by it.
package transport

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrPlaintextTransport is returned when a plaintext http/ws endpoint for a
// non-loopback host is requested without explicitly opting out of TLS.
var ErrPlaintextTransport = errors.New("refusing plaintext transport to non-loopback server without --insecure")

// ResolveServerURLs normalizes a user-supplied server address into canonical
// API (http(s)) and WS (ws(s)) base URLs with no trailing slash.
//
// A missing scheme defaults to https://. Explicit ws:// / wss:// schemes are
// mapped onto their HTTP counterparts for the API URL and back for the WS
// URL. Plaintext http endpoints are accepted only for loopback hosts or when
// allowInsecure is true; anything else fails with ErrPlaintextTransport
// before a single credential-bearing request is built.
func ResolveServerURLs(raw string, allowInsecure bool) (apiURL, wsURL string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("server URL is empty")
	}

	// A bare "host:port" would parse with the host as scheme — force a
	// scheme before parsing so scheme-less input is treated uniformly.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid server URL %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("invalid server URL %q: missing host", raw)
	}

	switch u.Scheme {
	case "http", "https":
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", "", fmt.Errorf("unsupported server URL scheme %q (use http:// or https://)", u.Scheme)
	}

	if u.Scheme == "http" && !allowInsecure && !IsLoopbackHost(u.Hostname()) {
		return "", "", fmt.Errorf("%w (got %s://%s)", ErrPlaintextTransport, u.Scheme, u.Host)
	}

	wsScheme := "wss"
	if u.Scheme == "http" {
		wsScheme = "ws"
	}

	apiURL = u.Scheme + "://" + u.Host
	wsURL = wsScheme + "://" + u.Host
	if u.Path != "" && u.Path != "/" {
		apiURL += u.Path
		wsURL += u.Path
	}
	return apiURL, wsURL, nil
}

// IsLoopbackHost reports whether host is a loopback name or address:
// localhost (any case, optional trailing dot), 127.0.0.0/8, or ::1.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
