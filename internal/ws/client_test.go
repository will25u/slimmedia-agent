package ws

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/transport"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.InitDB(t.TempDir())
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB
}

func TestNewClient_TransportPolicy(t *testing.T) {
	tests := []struct {
		name          string
		apiURL        string
		wsURL         string
		insecure      bool
		wantAPI       string
		wantWS        string
		wantPlaintext bool
	}{
		{name: "scheme-less defaults to TLS", apiURL: "media.example.com", wantAPI: "https://media.example.com", wantWS: "wss://media.example.com"},
		{name: "https preserved", apiURL: "https://media.example.com", wantAPI: "https://media.example.com", wantWS: "wss://media.example.com"},
		{name: "http loopback exempt", apiURL: "http://localhost:8082", wantAPI: "http://localhost:8082", wantWS: "ws://localhost:8082"},
		{name: "ws loopback exempt", wsURL: "ws://127.0.0.1:8082", wantAPI: "http://127.0.0.1:8082", wantWS: "ws://127.0.0.1:8082"},
		{name: "http non-loopback refused", apiURL: "http://media.example.com", wantPlaintext: true},
		{name: "ws LAN refused", wsURL: "ws://192.168.1.10:8082", wantPlaintext: true},
		{name: "http non-loopback allowed with insecure", apiURL: "http://media.example.com", insecure: true, wantAPI: "http://media.example.com", wantWS: "ws://media.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(Config{
				APIURL:   tt.apiURL,
				WSURL:    tt.wsURL,
				Insecure: tt.insecure,
				DB:       testDB(t),
			})
			defer func() {
				if client != nil {
					client.Stop()
				}
			}()

			if tt.wantPlaintext {
				if err == nil {
					t.Fatalf("NewClient succeeded for plaintext non-loopback URL; want refusal")
				}
				if !errors.Is(err, transport.ErrPlaintextTransport) {
					t.Fatalf("NewClient error = %v, want ErrPlaintextTransport", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewClient unexpected error: %v", err)
			}
			if client.config.APIURL != tt.wantAPI || client.config.WSURL != tt.wantWS {
				t.Fatalf("normalized URLs = (%q, %q), want (%q, %q)", client.config.APIURL, client.config.WSURL, tt.wantAPI, tt.wantWS)
			}
		})
	}
}

// TestFetchToken_OverTLS exercises the credential path end to end at the URL
// handling level: an https:// API URL must work against a real TLS server.
func TestFetchToken_OverTLS(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/exchange" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" {
			// The exchange call authenticates with the API key in the body.
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["api_key"] == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"jwt-token-from-tls-server"}`))
	}))
	defer ts.Close()

	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	tlsConf := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}

	client, err := NewClient(Config{
		APIURL:          ts.URL,
		TLSClientConfig: tlsConf,
		DB:              testDB(t),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Stop()

	client.config.APIKey = "test-api-key"
	if err := client.fetchToken(); err != nil {
		t.Fatalf("fetchToken over TLS failed: %v", err)
	}
	client.mu.Lock()
	token := client.token
	client.mu.Unlock()
	if token != "jwt-token-from-tls-server" {
		t.Fatalf("token = %q, want %q", token, "jwt-token-from-tls-server")
	}
}

// startTestWSClient stands up a TLS websocket echo server and starts a client
// against it, returning handles for the server connection and the client.
func startTestWSClient(t *testing.T, onConnect func(conn *websocket.Conn)) (*Client, chan struct{}) {
	t.Helper()

	connected := make(chan struct{}, 4)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/agent" {
			http.NotFound(w, r)
			return
		}
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connected <- struct{}{}
		if onConnect != nil {
			onConnect(conn)
		}
		// Keep the handler alive until the client goes away.
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(ts.Close)

	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	tlsConf := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}

	wsURL := "wss" + strings.TrimPrefix(ts.URL, "https")
	client, err := NewClient(Config{
		WSURL:           wsURL,
		TLSClientConfig: tlsConf,
		DB:              testDB(t),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// maintainConnection only dials once a token is present.
	client.mu.Lock()
	client.token = "test-token"
	client.mu.Unlock()

	client.Start()
	t.Cleanup(client.Stop)
	return client, connected
}

func waitForConnectionDrop(t *testing.T, client *Client, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		conn := client.conn
		client.mu.Unlock()
		if conn == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("client connection was not dropped within %v", timeout)
}

// Positive: a normal message well under the read limit is delivered.
func TestClient_MessageUnderReadLimitDelivered(t *testing.T) {
	env := protocol.Envelope{
		Type:    "job.start",
		ID:      "cmd-1",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"job_id":"cmd-1"}`),
	}
	payload, _ := json.Marshal(env)

	client, connected := startTestWSClient(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, payload)
	})
	defer client.Stop()

	handled := make(chan protocol.Envelope, 1)
	client.SetCommandHandler(func(env protocol.Envelope) {
		handled <- env
	})

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the client connect")
	}

	select {
	case got := <-handled:
		if got.ID != "cmd-1" {
			t.Fatalf("handled envelope ID = %q, want cmd-1", got.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message under the read limit was not delivered to the handler")
	}
}

// Negative: a malicious server sending a message over the 1 MiB read limit
// must get its connection dropped; the agent survives and keeps running.
func TestClient_MessageOverReadLimitDropped(t *testing.T) {
	oversized := bytes.Repeat([]byte("A"), maxMessageSize+1024)

	client, connected := startTestWSClient(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, oversized)
	})
	defer client.Stop()

	handled := make(chan protocol.Envelope, 1)
	client.SetCommandHandler(func(env protocol.Envelope) {
		handled <- env
	})

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the client connect")
	}

	// The read limit must kill the connection.
	waitForConnectionDrop(t, client, 10*time.Second)

	// The oversized payload must never reach the command handler.
	select {
	case env := <-handled:
		t.Fatalf("oversized message was delivered to handler: %+v", env)
	default:
	}

	// The agent is still alive and functional.
	if status := client.GetStatus(); status != "IDLE" {
		t.Fatalf("agent status = %q, want IDLE", status)
	}
}

// TestHandlerConcurrencyCapped floods the client with commands whose handler
// blocks; the number of in-flight handler goroutines must stay capped.
func TestHandlerConcurrencyCapped(t *testing.T) {
	envPayload, _ := json.Marshal(protocol.Envelope{Type: "job.start", ID: "x", Payload: json.RawMessage(`{}`)})

	release := make(chan struct{})
	var inflightMu sync.Mutex
	maxInflight := 0
	inflight := 0

	client, connected := startTestWSClient(t, func(conn *websocket.Conn) {
		for i := 0; i < 64; i++ {
			_ = conn.WriteMessage(websocket.TextMessage, envPayload)
		}
	})
	defer client.Stop()

	client.SetCommandHandler(func(protocol.Envelope) {
		inflightMu.Lock()
		inflight++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		inflightMu.Unlock()
		<-release // block until the test releases all handlers
		inflightMu.Lock()
		inflight--
		inflightMu.Unlock()
	})

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the client connect")
	}

	// Wait until the cap is saturated.
	deadline := time.Now().Add(5 * time.Second)
	for {
		inflightMu.Lock()
		n := inflight
		inflightMu.Unlock()
		if n >= maxConcurrentHandlers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d handlers in flight; expected saturation at %d", n, maxConcurrentHandlers)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)

	time.Sleep(100 * time.Millisecond)
	inflightMu.Lock()
	peak := maxInflight
	inflightMu.Unlock()
	if peak > maxConcurrentHandlers {
		t.Fatalf("handler concurrency peaked at %d, cap is %d", peak, maxConcurrentHandlers)
	}
}

// TestCancellationBypassesHandlerSaturation verifies that CANCEL_JOB commands
// are executed even when the normal handler worker queue is fully saturated.
func TestCancellationBypassesHandlerSaturation(t *testing.T) {
	startPayload, _ := json.Marshal(protocol.Envelope{Type: "job.start", ID: "start-1", Payload: json.RawMessage(`{}`)})
	cancelPayload, _ := json.Marshal(protocol.Envelope{Type: protocol.TypeCancelJob, ID: "cancel-1", Payload: json.RawMessage(`{"job_id":"job-1"}`)})

	release := make(chan struct{})
	defer close(release)
	var inflightMu sync.Mutex
	inflight := 0
	cancelHandled := make(chan struct{}, 1)

	client, connected := startTestWSClient(t, func(conn *websocket.Conn) {
		for i := 0; i < maxConcurrentHandlers; i++ {
			_ = conn.WriteMessage(websocket.TextMessage, startPayload)
		}
		// Wait until handler capacity is saturated
		for {
			inflightMu.Lock()
			n := inflight
			inflightMu.Unlock()
			if n >= maxConcurrentHandlers {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		// Now send CANCEL_JOB while queue is saturated
		_ = conn.WriteMessage(websocket.TextMessage, cancelPayload)
	})
	defer client.Stop()

	client.SetCommandHandler(func(env protocol.Envelope) {
		if env.Type == protocol.TypeCancelJob {
			select {
			case cancelHandled <- struct{}{}:
			default:
			}
			return
		}
		inflightMu.Lock()
		inflight++
		inflightMu.Unlock()
		<-release
		inflightMu.Lock()
		inflight--
		inflightMu.Unlock()
	})

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw client connect")
	}

	select {
	case <-cancelHandled:
		// Succeeded: cancellation was prioritized despite saturation
	case <-time.After(3 * time.Second):
		t.Fatal("CANCEL_JOB was dropped or blocked by saturated worker queue")
	}
}

// TestPriorityHandlerConcurrencyBounded floods the client with priority cancellation
// commands whose handler blocks; the number of in-flight priority handler goroutines
// must stay capped at maxConcurrentPriorityHandlers rather than spawning unmetered goroutines.
func TestPriorityHandlerConcurrencyBounded(t *testing.T) {
	cancelPayload, _ := json.Marshal(protocol.Envelope{Type: protocol.TypeCancelJob, ID: "cancel-x", Payload: json.RawMessage(`{"job_id":"job-x"}`)})

	release := make(chan struct{})
	var inflightMu sync.Mutex
	maxInflight := 0
	inflight := 0

	client, connected := startTestWSClient(t, func(conn *websocket.Conn) {
		for i := 0; i < 64; i++ {
			_ = conn.WriteMessage(websocket.TextMessage, cancelPayload)
		}
	})
	defer client.Stop()

	client.SetCommandHandler(func(env protocol.Envelope) {
		inflightMu.Lock()
		inflight++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		inflightMu.Unlock()
		<-release
		inflightMu.Lock()
		inflight--
		inflightMu.Unlock()
	})

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the client connect")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		inflightMu.Lock()
		n := inflight
		inflightMu.Unlock()
		if n >= maxConcurrentPriorityHandlers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d handlers in flight; expected saturation at %d", n, maxConcurrentPriorityHandlers)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Give any excess unmetered goroutines a moment to spin up and inflate maxInflight
	time.Sleep(100 * time.Millisecond)
	close(release)

	time.Sleep(100 * time.Millisecond)
	inflightMu.Lock()
	peak := maxInflight
	inflightMu.Unlock()
	if peak > maxConcurrentPriorityHandlers {
		t.Fatalf("priority handler concurrency peaked at %d, cap is %d", peak, maxConcurrentPriorityHandlers)
	}
}

// TestCancellationBurstHandling verifies that high-volume bursts of priority
// cancellation commands (CANCEL_JOB, CANCEL_AGENT_UPDATE) are safely bounded
// and processed without panics or memory leaks.
func TestCancellationBurstHandling(t *testing.T) {
	cancelJobPayload, _ := json.Marshal(protocol.Envelope{
		Type:    protocol.TypeCancelJob,
		ID:      "cancel-burst-job",
		Payload: json.RawMessage(`{"job_id":"job-burst"}`),
	})
	cancelUpdatePayload, _ := json.Marshal(protocol.Envelope{
		Type: protocol.TypeCancelAgentUpdate,
		ID:   "cancel-burst-update",
	})

	var inflightMu sync.Mutex
	maxInflight := 0
	inflight := 0
	handledCount := 0

	burstCount := 100
	client, connected := startTestWSClient(t, func(conn *websocket.Conn) {
		for i := 0; i < burstCount; i++ {
			payload := cancelJobPayload
			if i%2 == 1 {
				payload = cancelUpdatePayload
			}
			_ = conn.WriteMessage(websocket.TextMessage, payload)
		}
	})
	defer client.Stop()

	client.SetCommandHandler(func(env protocol.Envelope) {
		inflightMu.Lock()
		inflight++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		handledCount++
		inflightMu.Unlock()

		time.Sleep(5 * time.Millisecond)

		inflightMu.Lock()
		inflight--
		inflightMu.Unlock()
	})

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw client connect")
	}

	time.Sleep(300 * time.Millisecond)

	inflightMu.Lock()
	peak := maxInflight
	totalHandled := handledCount
	inflightMu.Unlock()

	if peak > maxConcurrentPriorityHandlers {
		t.Fatalf("priority handler concurrency peaked at %d, cap is %d", peak, maxConcurrentPriorityHandlers)
	}

	if totalHandled == 0 {
		t.Fatal("no priority cancellation commands were handled during burst")
	}
}

// TestFetchToken_OversizedSuccessBodyRejected pins that the token-exchange
// success decode is bounded like the error path: a server dripping an
// oversized 200 body fails the exchange instead of being buffered without
// limit.
func TestFetchToken_OversizedSuccessBodyRejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Unterminated JSON string that exceeds the response cap.
		_, _ = w.Write([]byte(`{"token":"` + strings.Repeat("x", maxTokenResponseBytes) + `"}`))
	}))
	defer ts.Close()

	c, err := NewClient(Config{
		APIKey: "sm_test_key",
		APIURL: ts.URL,
		WSURL:  "ws" + strings.TrimPrefix(ts.URL, "http"),
		DB:     testDB(t),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer c.Stop()

	if err := c.fetchToken(); err == nil {
		t.Fatal("fetchToken accepted an oversized success body")
	}

	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token != "" {
		t.Fatalf("token = %q, want empty after oversized response", token)
	}
}

func TestFetchToken_ErrAgentDeleted(t *testing.T) {
	deletedCh := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "ERR_AGENT_DELETED"})
	}))
	defer ts.Close()

	c, err := NewClient(Config{
		APIKey: "sm_test_key",
		APIURL: ts.URL,
		WSURL:  "ws" + strings.TrimPrefix(ts.URL, "http"),
		DB:     testDB(t),
		OnAgentDeleted: func() {
			select {
			case deletedCh <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer c.Stop()

	err = c.fetchToken()
	if !errors.Is(err, ErrAgentDeleted) {
		t.Fatalf("expected ErrAgentDeleted, got %v", err)
	}
}

func TestMaintainConnection_CloseCode4003(t *testing.T) {
	deletedCh := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ws/agent") {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			// Immediately send close code 4003
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(4003, "agent deleted"),
				time.Now().Add(time.Second),
			)
			_ = conn.Close()
			return
		}
		// Return valid token on exchange
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-jwt-token"})
	}))
	defer ts.Close()

	c, err := NewClient(Config{
		APIKey: "sm_test_key",
		APIURL: ts.URL,
		WSURL:  "ws" + strings.TrimPrefix(ts.URL, "http"),
		DB:     testDB(t),
		OnAgentDeleted: func() {
			select {
			case deletedCh <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer c.Stop()

	c.token = "test-token"
	go c.maintainConnection()

	select {
	case <-deletedCh:
		// Success!
	case <-time.After(3 * time.Second):
		t.Fatal("expected OnAgentDeleted callback on close code 4003")
	}

	// Verify connection is stopped and deleted is true
	c.mu.Lock()
	deleted := c.deleted
	c.mu.Unlock()
	if !deleted {
		t.Fatal("expected client.deleted to be true")
	}
}

func TestFormatConnectionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
		{
			name: "websocket server shutting down",
			err:  &websocket.CloseError{Code: websocket.CloseGoingAway, Text: "server shutting down"},
			want: "server shutting down",
		},
		{
			name: "websocket server restarting",
			err:  &websocket.CloseError{Code: websocket.CloseGoingAway, Text: "going away"},
			want: "server restarting",
		},
		{
			name: "websocket abnormal closure",
			err:  &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "unexpected EOF"},
			want: "connection dropped",
		},
		{
			name: "websocket normal closure",
			err:  &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "normal"},
			want: "connection closed normally",
		},
		{
			name: "websocket code 4003",
			err:  &websocket.CloseError{Code: 4003, Text: "agent deleted"},
			want: "agent deleted by server",
		},
		{
			name: "connection refused",
			err:  errors.New("dial tcp 127.0.0.1:8082: connect: connection refused"),
			want: "server unreachable (connection refused)",
		},
		{
			name: "timeout",
			err:  errors.New("i/o timeout"),
			want: "connection timed out",
		},
		{
			name: "bad handshake",
			err:  errors.New("websocket: bad handshake"),
			want: "authentication failed",
		},
		{
			name: "agent deleted",
			err:  ErrAgentDeleted,
			want: "agent deleted or invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatConnectionError(tt.err)
			if got != tt.want {
				t.Errorf("formatConnectionError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMaintainConnection_RejectsOldTokenOn403_TriggersAgentDeleted(t *testing.T) {
	deletedCh := make(chan struct{}, 1)
	connDetailCh := make(chan string, 10)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ws/agent") {
			// Simulate server rejecting WebSocket dial with HTTP 403 Forbidden
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden: agent is not active"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/api/v1/auth/exchange") {
			// Simulate wiped database: API key not found
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "invalid api key",
				"code":  "ERR_AGENT_DELETED",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c, err := NewClient(Config{
		APIKey: "sm_wiped_agent_key",
		APIURL: ts.URL,
		WSURL:  "ws" + strings.TrimPrefix(ts.URL, "http"),
		DB:     testDB(t),
		OnConnectionChange: func(connected bool, detail string) {
			if !connected {
				select {
				case connDetailCh <- detail:
				default:
				}
			}
		},
		OnAgentDeleted: func() {
			select {
			case deletedCh <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer c.Stop()

	// Pretend the agent held a pre-wipe JWT
	c.token = "stale-pre-wipe-jwt"
	go c.maintainConnection()

	select {
	case <-deletedCh:
		// Succeeded: agent recognized deletion and terminated reconnect loop
	case <-time.After(3 * time.Second):
		t.Fatal("expected OnAgentDeleted callback when 403 triggers token check that returns ERR_AGENT_DELETED")
	}

	c.mu.Lock()
	deleted := c.deleted
	c.mu.Unlock()
	if !deleted {
		t.Fatal("expected client.deleted to be true")
	}
}

func TestMaintainConnection_RejectsFreshTokenOn403_TriggersAgentDeleted(t *testing.T) {
	deletedCh := make(chan struct{}, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ws/agent") {
			// Server rejects connection as inactive
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden: agent is not active"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/api/v1/auth/exchange") {
			// Server exchanges token (e.g. user key with no agent row)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "token-without-active-agent"})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c, err := NewClient(Config{
		APIKey: "sm_test_key",
		APIURL: ts.URL,
		WSURL:  "ws" + strings.TrimPrefix(ts.URL, "http"),
		DB:     testDB(t),
		OnAgentDeleted: func() {
			select {
			case deletedCh <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer c.Stop()

	// Initial dial has fresh token; 403 on fresh token must trigger agent deleted
	go c.maintainConnection()

	select {
	case <-deletedCh:
		// Success
	case <-time.After(3 * time.Second):
		t.Fatal("expected OnAgentDeleted callback when fresh token is rejected with 403 on /ws/agent")
	}

	c.mu.Lock()
	deleted := c.deleted
	c.mu.Unlock()
	if !deleted {
		t.Fatal("expected client.deleted to be true")
	}
}

func TestMaintainConnection_DialFailureUpdatesConnectionDetail(t *testing.T) {
	detailCh := make(chan string, 5)

	// Use a non-listening port to force dial failure
	c, err := NewClient(Config{
		APIKey: "sm_test_key",
		APIURL: "http://127.0.0.1:54321",
		WSURL:  "ws://127.0.0.1:54321",
		DB:     testDB(t),
		OnConnectionChange: func(connected bool, detail string) {
			if !connected && detail != "" {
				select {
				case detailCh <- detail:
				default:
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer c.Stop()

	c.token = "fake-token"
	go c.maintainConnection()

	select {
	case detail := <-detailCh:
		if !strings.Contains(detail, "unreachable") && !strings.Contains(detail, "connection refused") {
			t.Fatalf("unexpected detail: %q", detail)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected OnConnectionChange callback with detail on dial failure")
	}
}

func TestIsConnectionReplaced(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "close going away with connection replaced",
			err:      &websocket.CloseError{Code: websocket.CloseGoingAway, Text: "connection replaced"},
			expected: true,
		},
		{
			name:     "close code 1001 with mixed case connection replaced",
			err:      &websocket.CloseError{Code: 1001, Text: "Connection Replaced by another worker"},
			expected: true,
		},
		{
			name:     "close policy violation with connection replaced",
			err:      &websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: "connection replaced"},
			expected: true,
		},
		{
			name:     "formatted string error containing connection replaced",
			err:      errors.New("websocket: close 1001 (going away): connection replaced"),
			expected: true,
		},
		{
			name:     "close going away server restarting",
			err:      &websocket.CloseError{Code: websocket.CloseGoingAway, Text: "server restarting"},
			expected: false,
		},
		{
			name:     "other error",
			err:      errors.New("i/o timeout"),
			expected: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isConnectionReplaced(tc.err)
			if got != tc.expected {
				t.Errorf("isConnectionReplaced(%v) = %v, want %v", tc.err, got, tc.expected)
			}
		})
	}
}

func TestMaintainConnection_ConnectionReplacedCircuitBreaker(t *testing.T) {
	trippedCh := make(chan struct{}, 1)
	connectCount := 0
	var mu sync.Mutex

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ws/agent") {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			mu.Lock()
			connectCount++
			mu.Unlock()

			// Send close frame 1001 (going away) with "connection replaced"
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "connection replaced"),
				time.Now().Add(time.Second),
			)
			conn.Close()
			return
		}
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	c, err := NewClient(Config{
		APIKey:          "sm_test_key",
		APIURL:          ts.URL,
		WSURL:           wsURL,
		DB:              testDB(t),
		ReplacedBackoff: 10 * time.Millisecond,
		OnCircuitBreakerTripped: func() {
			select {
			case trippedCh <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer c.Stop()

	c.token = "fake-token"
	done := make(chan struct{})
	go func() {
		c.maintainConnection()
		close(done)
	}()

	select {
	case <-trippedCh:
		// Circuit breaker tripped!
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for circuit breaker to trip")
	}

	// maintainConnection must return and stop reconnecting
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintainConnection did not exit after circuit breaker tripped")
	}

	if !c.IsCircuitBreakerTripped() {
		t.Fatal("expected IsCircuitBreakerTripped() to return true")
	}

	status, _ := c.GetStatusWithActivity()
	if status != "PAUSED" {
		t.Fatalf("expected status PAUSED, got %q", status)
	}

	mu.Lock()
	count := connectCount
	mu.Unlock()
	if count != 3 {
		t.Fatalf("expected exactly 3 connection attempts before circuit breaker tripped, got %d", count)
	}
}

func TestHeartbeatInterval_IdleAndWorking(t *testing.T) {
	c, err := NewClient(Config{
		APIURL:   "http://localhost:8082",
		WSURL:    "ws://localhost:8082/api/v1/ws",
		Insecure: true,
		DB:       testDB(t),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer c.Stop()

	// Initial status is IDLE, interval should be 30s
	if interval := c.CurrentHeartbeatInterval(); interval != DefaultIdleHeartbeatInterval {
		t.Fatalf("idle heartbeat interval = %v, want %v", interval, DefaultIdleHeartbeatInterval)
	}

	// Status transition to WORKING relaxes/tightens interval to 10s
	c.SetStatusWithActivity("WORKING", "transcoding")
	if interval := c.CurrentHeartbeatInterval(); interval != DefaultActiveHeartbeatInterval {
		t.Fatalf("active heartbeat interval = %v, want %v", interval, DefaultActiveHeartbeatInterval)
	}

	// Returning to IDLE restores 30s interval
	c.SetStatusWithActivity("IDLE", "")
	if interval := c.CurrentHeartbeatInterval(); interval != DefaultIdleHeartbeatInterval {
		t.Fatalf("restored idle heartbeat interval = %v, want %v", interval, DefaultIdleHeartbeatInterval)
	}
}



