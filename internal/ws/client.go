package ws

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/transport"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// ErrAgentDeleted indicates the server rejected the agent because its key was deleted/revoked.
var ErrAgentDeleted = errors.New("agent deleted by server")

// maxMessageSize bounds a single websocket frame/message the agent will
// buffer; a malicious server must not be able to OOM the agent.
const maxMessageSize = protocol.WSMaxMessageSize

// Keepalive bounds shared across client and server via internal/protocol.
// wsPongWait is a variable so tests can shorten the window.
const (
	wsPingPeriod = protocol.WSPingPeriod
	wsWriteWait  = protocol.WSWriteWait
)

var (
	wsPongWait              = protocol.WSPongWait
	offlineReplayAckTimeout = 5 * time.Second
	maxOfflineSummaries     = int64(1000)
	offlineSummaryTTL       = 7 * 24 * time.Hour
)

// setupWSConn bounds inbound frames and arms the read deadline; pongs from
// the server extend it, so a silent server fails the read within wsPongWait
// and the reconnect loop runs.
func setupWSConn(conn *websocket.Conn) {
	conn.SetReadLimit(maxMessageSize)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
}

// pingLoop sends client pings until stop is closed (one per connection);
// writeMu serializes pings with the rest of the client's writes.
func (c *Client) pingLoop(conn *websocket.Conn, stop <-chan struct{}) {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			err := conn.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// maxTokenResponseBytes caps the auth-exchange response body the agent will
// decode, matching the error path's 4096-byte limit: the token payload is a
// small JSON object, and a slow-drip server must not inflate memory for the
// duration of the client timeout.
const maxTokenResponseBytes = 4096

// maxConcurrentHandlers caps goroutines spawned to process inbound commands
// so a flood of messages cannot exhaust memory or scheduler capacity.
const maxConcurrentHandlers = 8

// maxConcurrentPriorityHandlers caps goroutines spawned to process inbound
// priority cancellation commands (e.g. CANCEL_JOB, CANCEL_AGENT_UPDATE) so
// bursty streams or flood attacks cannot exhaust memory or scheduler capacity.
const maxConcurrentPriorityHandlers = 8

// defaultHTTPClient bounds token-exchange and other API calls. A hung server
// must not stall token rotation forever.
var defaultHTTPClient = &http.Client{Timeout: 15 * time.Second}

const (
	// DefaultActiveHeartbeatInterval is the cadence for WebSocket heartbeats while jobs are actively running.
	DefaultActiveHeartbeatInterval = 10 * time.Second
	// DefaultIdleHeartbeatInterval is the relaxed cadence for WebSocket heartbeats while idle.
	DefaultIdleHeartbeatInterval = 30 * time.Second
)

type Config struct {
	APIKey    string
	APIURL    string
	WSURL     string
	AgentName string
	Version   string
	// Insecure permits plaintext http/ws endpoints for non-loopback hosts.
	// Certificate verification is never disabled by this flag.
	Insecure              bool
	HeartbeatInterval     time.Duration
	IdleHeartbeatInterval time.Duration
	DB                    *sql.DB
	// TLSClientConfig overrides TLS settings for the wss dial and the API
	// HTTP client (e.g. a private CA for self-hosted deployments). When nil,
	// wss dials require TLS 1.3+ and the system trust store is used.
	TLSClientConfig *tls.Config
	// OnConnectionChange, when set, is called on WebSocket connection
	// transitions: connected=true after the dial succeeds and connected=false
	// when the connection drops (detail carries the disconnect cause). It is
	// called from the connection maintenance goroutine and must not block.
	OnConnectionChange func(connected bool, detail string)
	// OnAgentDeleted, when set, is called when the server indicates that the
	// agent was deleted or unlinked (via HTTP 401 ERR_AGENT_DELETED or WS close code 4003).
	OnAgentDeleted func()
	// OnSetLibraryPath, when set, is called when a SET_LIBRARY_PATH frame is received.
	OnSetLibraryPath func(payload protocol.SetLibraryPathPayload)
	// ReplacedBackoff overrides the base backoff delay when disconnected due to session replacement.
	ReplacedBackoff time.Duration
	// OnCircuitBreakerTripped is called when 3 consecutive connection replaced disconnects trip the breaker.
	OnCircuitBreakerTripped func()
}

type CommandHandler func(env protocol.Envelope)

type Client struct {
	config                Config
	token                 string
	conn                  *websocket.Conn
	status                string
	activity              string
	updateScheduled       bool
	deleted               bool
	circuitBreakerTripped bool
	mu                    sync.Mutex
	writeMu         sync.Mutex
	replayMu        sync.Mutex
	ackMu           sync.Mutex
	ackWaiters      map[string]chan protocol.Envelope
	isReading       bool
	ctx             context.Context
	cancel          context.CancelFunc

	onCommand CommandHandler

	// handlerSem gates handler goroutines spawned per inbound message.
	handlerSem chan struct{}

	// prioritySem gates handler goroutines spawned for priority cancellation messages.
	prioritySem chan struct{}

	httpClient *http.Client

	// Queues for sending messages
	sendQueue chan protocol.Envelope

	// hbResetChan notifies heartbeatLoop of status transitions to adjust interval
	hbResetChan chan struct{}
}

// normalizeURLs applies the shared transport policy to the configured
// addresses: TLS by default, plaintext only to loopback or with Insecure.
// When only one of the pair is set it falls back to the other.
func normalizeURLs(rawWS, rawAPI string, insecure bool) (string, string, error) {
	rawWS = strings.TrimSpace(rawWS)
	rawAPI = strings.TrimSpace(rawAPI)
	if rawWS == "" && rawAPI == "" {
		return "", "", errors.New("no server URL configured")
	}
	if rawWS == "" {
		rawWS = rawAPI
	}
	if rawAPI == "" {
		rawAPI = rawWS
	}

	_, wsResolved, err := transport.ResolveServerURLs(rawWS, insecure)
	if err != nil {
		return "", "", err
	}
	apiResolved, _, err := transport.ResolveServerURLs(rawAPI, insecure)
	if err != nil {
		return "", "", err
	}
	return wsResolved, apiResolved, nil
}

func NewClient(cfg Config) (*Client, error) {
	wsURL, apiURL, err := normalizeURLs(cfg.WSURL, cfg.APIURL, cfg.Insecure)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %w", err)
	}
	cfg.WSURL = wsURL
	cfg.APIURL = apiURL

	httpClient := defaultHTTPClient
	if cfg.TLSClientConfig != nil {
		httpClient = &http.Client{
			Timeout:   defaultHTTPClient.Timeout,
			Transport: &http.Transport{TLSClientConfig: cfg.TLSClientConfig},
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		config:      cfg,
		status:      "IDLE",
		ctx:         ctx,
		cancel:      cancel,
		handlerSem:  make(chan struct{}, maxConcurrentHandlers),
		prioritySem: make(chan struct{}, maxConcurrentPriorityHandlers),
		httpClient:  httpClient,
		sendQueue:   make(chan protocol.Envelope, 100),
		hbResetChan: make(chan struct{}, 1),
		ackWaiters:  make(map[string]chan protocol.Envelope),
	}, nil
}

func (c *Client) Start() {
	go c.maintainConnection()
	go c.tokenRotation()
	go c.writeLoop()
	go c.heartbeatLoop()
}

func (c *Client) Stop() {
	c.cancel()
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
}

func (c *Client) Send(env protocol.Envelope) {
	select {
	case c.sendQueue <- env:
	default:
		log.Printf("Outbound queue full, dropping message type %s", env.Type)
		if env.Type == protocol.TypeFileComplete {
			c.queueOffline(env)
		}
	}
}

func (c *Client) SetStatusWithActivity(status, activity string) {
	c.mu.Lock()
	c.status = status
	c.activity = activity
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		c.sendHeartbeat()
	}
	select {
	case c.hbResetChan <- struct{}{}:
	default:
	}
}

func (c *Client) GetStatus() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == "" {
		return "IDLE"
	}
	return c.status
}

// GetStatusWithActivity returns the current agent status and activity text.
func (c *Client) GetStatusWithActivity() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := c.status
	if status == "" {
		status = "IDLE"
	}
	return status, c.activity
}

// GetUpdateScheduled reports whether an agent update is scheduled or in
// progress, as reflected in heartbeats.
func (c *Client) GetUpdateScheduled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updateScheduled
}

// IsConnected reports whether the WebSocket is currently connected.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// IsCircuitBreakerTripped reports whether the reconnection circuit breaker has tripped.
func (c *Client) IsCircuitBreakerTripped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.circuitBreakerTripped
}

// Token returns the current authentication token, fetching one if not yet present.
func (c *Client) Token() string {
	c.mu.Lock()
	t := c.token
	c.mu.Unlock()
	if t != "" {
		return t
	}
	_ = c.fetchToken()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// notifyConnectionChange invokes the OnConnectionChange callback, if set.
func (c *Client) notifyConnectionChange(connected bool, detail string) {
	if cb := c.config.OnConnectionChange; cb != nil {
		cb(connected, detail)
	}
}

func (c *Client) SetCommandHandler(handler CommandHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onCommand = handler
}

func (c *Client) SetUpdateScheduled(scheduled bool) {
	c.mu.Lock()
	c.updateScheduled = scheduled
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		c.sendHeartbeat()
	}
}

func (c *Client) sendHeartbeat() {
	c.mu.Lock()
	status := c.status
	activity := c.activity
	updateScheduled := c.updateScheduled
	name := c.config.AgentName
	c.mu.Unlock()

	if status == "" {
		status = "IDLE"
	}
	if name == "" {
		name = "slimmedia-agent"
	}

	version := c.config.Version
	if version == "" {
		version = "1.0.0"
	}
	platform := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)

	payload, err := json.Marshal(protocol.HeartbeatPayload{
		AgentName:       name,
		Status:          status,
		Version:         version,
		Platform:        platform,
		Activity:        activity,
		UpdateScheduled: updateScheduled,
	})
	if err != nil {
		return
	}

	env := protocol.Envelope{
		Type:    "heartbeat",
		ID:      fmt.Sprintf("hb-%d", time.Now().UnixNano()),
		TS:      time.Now(),
		Payload: payload,
	}

	c.Send(env)
}

// CurrentHeartbeatInterval returns the effective heartbeat interval given the client's current status.
func (c *Client) CurrentHeartbeatInterval() time.Duration {
	return c.currentHeartbeatInterval()
}

func (c *Client) currentHeartbeatInterval() time.Duration {
	c.mu.Lock()
	status := c.status
	c.mu.Unlock()

	if strings.EqualFold(status, "WORKING") {
		if c.config.HeartbeatInterval > 0 {
			return c.config.HeartbeatInterval
		}
		return DefaultActiveHeartbeatInterval
	}

	if c.config.IdleHeartbeatInterval > 0 {
		return c.config.IdleHeartbeatInterval
	}
	if c.config.HeartbeatInterval > 0 {
		return c.config.HeartbeatInterval
	}
	return DefaultIdleHeartbeatInterval
}

func (c *Client) heartbeatLoop() {
	for {
		interval := c.currentHeartbeatInterval()
		timer := time.NewTimer(interval)

		select {
		case <-c.ctx.Done():
			timer.Stop()
			return
		case <-c.hbResetChan:
			timer.Stop()
		case <-timer.C:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn != nil {
				c.sendHeartbeat()
			}
		}
	}
}

func (c *Client) handleAgentDeleted() {
	c.mu.Lock()
	if c.deleted {
		c.mu.Unlock()
		return
	}
	c.deleted = true
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

	c.notifyConnectionChange(false, "agent deleted by server")

	// Attempt to flush and acknowledge any remaining offline summaries before tearing down
	if conn != nil {
		c.replayOfflineSummariesWithConn(conn)
	}

	c.cancel()
	if conn != nil {
		_ = conn.Close()
	}

	if c.config.OnAgentDeleted != nil {
		c.config.OnAgentDeleted()
	}
}

func (c *Client) fetchToken() error {
	payload := map[string]string{"api_key": c.config.APIKey}
	b, _ := json.Marshal(payload)

	apiURL := strings.TrimSuffix(c.config.APIURL, "/")
	req, err := http.NewRequestWithContext(c.ctx, "POST", apiURL+"/api/v1/auth/exchange", bytes.NewBuffer(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			var errRes struct {
				Code string `json:"code"`
			}
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
			_ = json.Unmarshal(bodyBytes, &errRes)
			if errRes.Code == "ERR_AGENT_DELETED" || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return ErrAgentDeleted
			}
		}
		return fmt.Errorf("auth exchange failed: status %d", resp.StatusCode)
	}

	// Bound the success decode like the error path: the token payload is a
	// small JSON object, so an oversized body means a misbehaving server.
	var res struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponseBytes)).Decode(&res); err != nil {
		return err
	}

	c.mu.Lock()
	c.token = res.Token
	c.mu.Unlock()
	return nil
}

func (c *Client) tokenRotation() {
	for {
		c.mu.Lock()
		t := c.token
		c.mu.Unlock()

		var waitTime time.Duration = 5 * time.Minute

		if t != "" {
			token, _, err := new(jwt.Parser).ParseUnverified(t, jwt.MapClaims{})
			if err == nil {
				if claims, ok := token.Claims.(jwt.MapClaims); ok {
					if exp, ok := claims["exp"].(float64); ok {
						expiresAt := time.Unix(int64(exp), 0)
						// Refresh 2 minutes before expiry
						refreshAt := expiresAt.Add(-2 * time.Minute)
						if time.Now().Before(refreshAt) {
							waitTime = time.Until(refreshAt)
						} else {
							waitTime = 10 * time.Second
						}
					}
				}
			}
		} else {
			waitTime = 1 * time.Second
		}

		select {
		case <-c.ctx.Done():
			return
		case <-time.After(waitTime):
			if err := c.fetchToken(); err != nil {
				if errors.Is(err, ErrAgentDeleted) {
					c.handleAgentDeleted()
					return
				}
				log.Printf("Failed to refresh token: %v", err)
			}
		}
	}
}

// formatConnectionError translates internal websocket, network, and protocol
// errors into concise, human-readable status text suitable for display in
// the GUI or CLI status lines.
func formatConnectionError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrAgentDeleted) {
		return "agent deleted or invalid"
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNormalClosure:
			return "connection closed normally"
		case websocket.CloseGoingAway:
			if strings.Contains(strings.ToLower(closeErr.Text), "connection replaced") {
				return "connection replaced by another agent instance"
			}
			if strings.Contains(strings.ToLower(closeErr.Text), "shutting down") {
				return "server shutting down"
			}
			return "server restarting"
		case websocket.CloseAbnormalClosure:
			return "connection dropped"
		case 4003:
			return "agent deleted by server"
		default:
			if closeErr.Text != "" {
				return closeErr.Text
			}
			return fmt.Sprintf("server closed connection (code %d)", closeErr.Code)
		}
	}

	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "connection refused"):
		return "server unreachable (connection refused)"
	case strings.Contains(low, "no such host"):
		return "server address not found"
	case strings.Contains(low, "i/o timeout") || strings.Contains(low, "deadline exceeded") || strings.Contains(low, "handshake timeout"):
		return "connection timed out"
	case strings.Contains(low, "certificate") || strings.Contains(low, "x509") || strings.Contains(low, "tls:"):
		return "TLS certificate error"
	case strings.Contains(low, "bad handshake") || strings.Contains(low, "status 401") || strings.Contains(low, "status 403"):
		return "authentication failed"
	case strings.Contains(low, "unexpected eof") || strings.Contains(low, "broken pipe") || strings.Contains(low, "reset by peer"):
		return "connection dropped"
	case strings.HasPrefix(msg, "websocket: close "):
		return strings.TrimPrefix(msg, "websocket: close ")
	case strings.HasPrefix(msg, "websocket: "):
		return strings.TrimPrefix(msg, "websocket: ")
	default:
		return msg
	}
}

// isConnectionReplaced reports whether err indicates the connection was terminated
// because another agent instance connected with the same credentials.
func isConnectionReplaced(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if (closeErr.Code == websocket.CloseGoingAway || closeErr.Code == 1001 || closeErr.Code == websocket.ClosePolicyViolation) &&
			strings.Contains(strings.ToLower(closeErr.Text), "connection replaced") {
			return true
		}
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "connection replaced")
}

func (c *Client) maintainConnection() {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second
	tokenRefreshed := false
	consecutiveReplaced := 0

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.mu.Lock()
		t := c.token
		deleted := c.deleted
		c.mu.Unlock()

		if deleted {
			return
		}

		if t == "" {
			if err := c.fetchToken(); err != nil {
				if errors.Is(err, ErrAgentDeleted) {
					c.notifyConnectionChange(false, "agent deleted by server")
					c.handleAgentDeleted()
					return
				}
				detail := formatConnectionError(err)
				c.notifyConnectionChange(false, detail)
				log.Printf("Failed to fetch token: %v (%s). Retrying in %v...", err, detail, backoff)
				select {
				case <-c.ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			tokenRefreshed = true
			c.mu.Lock()
			t = c.token
			c.mu.Unlock()
		}

		// Connect
		headers := http.Header{}
		headers.Set("Authorization", "Bearer "+t)

		wsURL := strings.TrimSuffix(c.config.WSURL, "/")
		dialer := *websocket.DefaultDialer
		dialer.HandshakeTimeout = 15 * time.Second
		if c.config.TLSClientConfig != nil {
			dialer.TLSClientConfig = c.config.TLSClientConfig
		} else if strings.HasPrefix(wsURL, "wss://") && dialer.TLSClientConfig == nil {
			dialer.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		}

		conn, resp, err := dialer.Dial(wsURL+"/ws/agent", headers)
		if err != nil {
			var connErrDetail string
			if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
				c.mu.Lock()
				c.token = ""
				c.mu.Unlock()

				// If we already refreshed the token in this cycle or if the server explicitly
				// rejected agent authorization (403 Forbidden on fresh token), unpair immediately.
				if resp.StatusCode == http.StatusForbidden && tokenRefreshed {
					c.notifyConnectionChange(false, "agent deleted or invalid")
					c.handleAgentDeleted()
					return
				}

				if fetchErr := c.fetchToken(); fetchErr != nil {
					if errors.Is(fetchErr, ErrAgentDeleted) {
						c.notifyConnectionChange(false, "agent deleted by server")
						c.handleAgentDeleted()
						return
					}
					connErrDetail = formatConnectionError(fetchErr)
				} else {
					tokenRefreshed = true
					connErrDetail = "authentication failed; re-authenticating"
				}
			} else {
				connErrDetail = formatConnectionError(err)
			}

			c.notifyConnectionChange(false, connErrDetail)
			log.Printf("WebSocket connection failed: %v (%s). Retrying in %v...", err, connErrDetail, backoff)
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// Bound inbound message size and arm the read deadline so a dead
		// peer is detected by a failed read, not just a failed write.
		setupWSConn(conn)

		log.Println("WebSocket connected")
		backoff = 1 * time.Second // Reset backoff
		tokenRefreshed = false

		pingStop := make(chan struct{})
		go c.pingLoop(conn, pingStop)

		c.mu.Lock()
		c.conn = conn
		c.isReading = true
		c.mu.Unlock()

		c.sendHeartbeat()
		c.notifyConnectionChange(true, "")

		// Replay offline summaries in background while reader loop is active
		go c.replayOfflineSummaries()

		// Wait for connection to drop and read incoming messages
		var disconnectErr error
		for {
			msgType, p, err := conn.ReadMessage()
			if err != nil {
				log.Printf("WebSocket disconnected: %v", err)
				disconnectErr = err
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) && closeErr.Code == 4003 {
					c.notifyConnectionChange(false, "agent deleted by server")
					c.handleAgentDeleted()
					return
				}
				break
			}

			if msgType == websocket.TextMessage {
				var env protocol.Envelope
				if err := json.Unmarshal(p, &env); err == nil {
					if env.Type == protocol.TypeFileCompleteAck || env.Type == "FILE_COMPLETE_ACK" {
						c.handleFileCompleteAck(env)
						c.notifyAckWaiter(env)
						continue
					}
					if env.Type == protocol.TypeSetLibraryPath && c.config.OnSetLibraryPath != nil {
						var slp protocol.SetLibraryPathPayload
						if err := json.Unmarshal(env.Payload, &slp); err == nil {
							c.config.OnSetLibraryPath(slp)
						}
						continue
					}
					c.mu.Lock()
					handler := c.onCommand
					c.mu.Unlock()
					if handler != nil {
						if isPriorityCommand(env.Type) {
							select {
							case c.prioritySem <- struct{}{}:
								go func(env protocol.Envelope) {
									defer func() { <-c.prioritySem }()
									handler(env)
								}(env)
							default:
								log.Printf("Dropping %s command %s: %d priority handlers already in flight", env.Type, env.ID, maxConcurrentPriorityHandlers)
							}
							continue
						}
						select {
						case c.handlerSem <- struct{}{}:
							go func(env protocol.Envelope) {
								defer func() { <-c.handlerSem }()
								handler(env)
							}(env)
						default:
							// Saturated — drop rather than spawn unbounded
							// goroutines; the server will retry if it matters.
							log.Printf("Dropping %s command %s: %d handlers already in flight", env.Type, env.ID, maxConcurrentHandlers)
						}
					}
				}
			}
		}

		close(pingStop)

		c.clearAckWaiters()

		c.mu.Lock()
		c.isReading = false
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()

		if isConnectionReplaced(disconnectErr) {
			consecutiveReplaced++
			if consecutiveReplaced >= 3 {
				log.Printf("ERROR: another active agent instance is using this agent token/ID; halting reconnect attempts (circuit breaker triggered after %d consecutive connection replaced disconnects)", consecutiveReplaced)
				c.mu.Lock()
				c.status = "PAUSED"
				c.circuitBreakerTripped = true
				c.mu.Unlock()
				c.notifyConnectionChange(false, "session usurped: another active agent instance is using this agent token/ID")
				if c.config.OnCircuitBreakerTripped != nil {
					c.config.OnCircuitBreakerTripped()
				}
				return
			}

			c.notifyConnectionChange(false, formatConnectionError(disconnectErr))
			baseBackoff := 5 * time.Second
			if c.config.ReplacedBackoff > 0 {
				baseBackoff = c.config.ReplacedBackoff
			}
			backoffDelay := baseBackoff * time.Duration(consecutiveReplaced)
			log.Printf("WebSocket connection replaced by another instance (%d/3). Backing off for %v...", consecutiveReplaced, backoffDelay)
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoffDelay):
			}
		} else {
			consecutiveReplaced = 0
			c.notifyConnectionChange(false, formatConnectionError(disconnectErr))
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}
		}
	}
}

func (c *Client) writeWS(conn *websocket.Conn, messageType int, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	return conn.WriteMessage(messageType, data)
}

func (c *Client) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			for {
				select {
				case env := <-c.sendQueue:
					c.queueOffline(env)
				default:
					return
				}
			}
		case env := <-c.sendQueue:
			// If not connected, save to DB
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn == nil {
				c.queueOffline(env)
				continue
			}

			b, err := json.Marshal(env)
			if err != nil {
				log.Printf("Failed to marshal envelope: %v", err)
				continue
			}

			if err := c.writeWS(conn, websocket.TextMessage, b); err != nil {
				// Save this message for later since write failed
				c.queueOffline(env)

				// Force disconnect to trigger reconnect
				c.mu.Lock()
				if c.conn != nil {
					c.conn.Close()
					c.conn = nil
				}
				c.mu.Unlock()
			}
		}
	}
}

func (c *Client) queueOffline(env protocol.Envelope) {
	if c.config.DB == nil {
		return
	}
	// We only care about file-completion summaries for offline queuing
	if env.Type == protocol.TypeFileComplete {
		b, err := json.Marshal(env)
		if err != nil {
			log.Printf("Failed to marshal offline envelope: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		q := db.New(c.config.DB)
		if err := q.InsertOfflineSummary(ctx, string(b)); err != nil {
			log.Printf("Failed to queue offline summary: %v", err)
			return
		}
		log.Printf("Queued offline summary to SQLite")

		// Prune expired summaries older than 7 days
		_ = q.PruneExpiredOfflineSummaries(ctx)
		// Prune oldest if queue exceeds bound
		_ = q.PruneOldestOfflineSummaries(ctx, maxOfflineSummaries)
	}
}

func (c *Client) notifyAckWaiter(env protocol.Envelope) {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	var ack protocol.FileCompleteAckPayload
	if len(env.Payload) > 0 {
		_ = json.Unmarshal(env.Payload, &ack)
	}
	for id, ch := range c.ackWaiters {
		if id == env.ID || (ack.EnvelopeID != "" && id == ack.EnvelopeID) {
			select {
			case ch <- env:
			default:
			}
		}
	}
}

func (c *Client) clearAckWaiters() {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	for id, ch := range c.ackWaiters {
		close(ch)
		delete(c.ackWaiters, id)
	}
}

func (c *Client) handleFileCompleteAck(env protocol.Envelope) {
	if c.config.DB == nil || env.ID == "" {
		return
	}
	if len(env.Payload) > 0 {
		var ack protocol.FileCompleteAckPayload
		if err := json.Unmarshal(env.Payload, &ack); err == nil && !ack.Success {
			log.Printf("Job completion failed on server for env=%s; retaining in offline queue for retry", env.ID)
			return
		}
	}
	summaries, err := db.New(c.config.DB).GetOfflineSummaries(c.ctx)
	if err != nil {
		return
	}
	for _, s := range summaries {
		var storedEnv protocol.Envelope
		if err := json.Unmarshal([]byte(s.Payload), &storedEnv); err == nil {
			if storedEnv.ID == env.ID {
				_ = db.New(c.config.DB).DeleteOfflineSummary(c.ctx, int64(s.ID))
				log.Printf("Acknowledged and deleted offline summary id=%d (env=%s)", s.ID, env.ID)
				break
			}
		}
	}
}

func (c *Client) replayOfflineSummaries() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}
	c.replayOfflineSummariesWithConn(conn)
}

func (c *Client) replayOfflineSummariesWithConn(conn *websocket.Conn) {
	if c.config.DB == nil || conn == nil {
		return
	}
	c.replayMu.Lock()
	defer c.replayMu.Unlock()

	q := db.New(c.config.DB)
	_ = q.PruneExpiredOfflineSummaries(c.ctx)
	_ = q.PruneOldestOfflineSummaries(c.ctx, maxOfflineSummaries)

	summaries, err := q.GetOfflineSummaries(c.ctx)
	if err != nil {
		log.Printf("Failed to get offline summaries: %v", err)
		return
	}

	for _, s := range summaries {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		var env protocol.Envelope
		if err := json.Unmarshal([]byte(s.Payload), &env); err != nil {
			log.Printf("Failed to unmarshal offline summary (id=%d): %v", s.ID, err)
			_ = q.DeleteOfflineSummary(c.ctx, int64(s.ID))
			continue
		}

		// Message expiration check: discard if older than TTL
		if !env.TS.IsZero() && time.Since(env.TS) > offlineSummaryTTL {
			log.Printf("Offline summary (id=%d, env=%s) expired (older than %v); discarding", s.ID, env.ID, offlineSummaryTTL)
			_ = q.DeleteOfflineSummary(c.ctx, int64(s.ID))
			continue
		}

		if err := c.writeWS(conn, websocket.TextMessage, []byte(s.Payload)); err != nil {
			log.Printf("Failed to replay offline summary: %v", err)
			break
		}

		c.mu.Lock()
		activeReader := c.isReading
		c.mu.Unlock()

		acked := false
		if activeReader {
			ackCh := make(chan protocol.Envelope, 1)
			c.ackMu.Lock()
			c.ackWaiters[env.ID] = ackCh
			c.ackMu.Unlock()

			select {
			case <-c.ctx.Done():
				c.ackMu.Lock()
				delete(c.ackWaiters, env.ID)
				c.ackMu.Unlock()
				return
			case ackEnv, ok := <-ackCh:
				c.ackMu.Lock()
				delete(c.ackWaiters, env.ID)
				c.ackMu.Unlock()
				if ok {
					var ack protocol.FileCompleteAckPayload
					if len(ackEnv.Payload) > 0 {
						if err := json.Unmarshal(ackEnv.Payload, &ack); err == nil {
							acked = ack.Success
						} else {
							acked = true
						}
					} else {
						acked = true
					}
				}
			case <-time.After(offlineReplayAckTimeout):
				c.ackMu.Lock()
				delete(c.ackWaiters, env.ID)
				c.ackMu.Unlock()
				log.Printf("Timed out waiting for server ACK for offline summary (id=%d, env=%s)", s.ID, env.ID)
			}
		} else {
			// Standalone connection without active reader loop (e.g. test harness)
			deadline := time.Now().Add(offlineReplayAckTimeout)
			_ = conn.SetReadDeadline(deadline)
			for time.Now().Before(deadline) {
				msgType, ackMsg, err := conn.ReadMessage()
				if err != nil {
					log.Printf("Failed to receive ACK for offline summary (id=%d): %v", s.ID, err)
					break
				}
				if msgType != websocket.TextMessage {
					continue
				}
				var incomingEnv protocol.Envelope
				if err := json.Unmarshal(ackMsg, &incomingEnv); err != nil {
					continue
				}
				if incomingEnv.Type == protocol.TypeFileCompleteAck || incomingEnv.Type == "FILE_COMPLETE_ACK" {
					var ack protocol.FileCompleteAckPayload
					success := true
					if len(incomingEnv.Payload) > 0 {
						if err := json.Unmarshal(incomingEnv.Payload, &ack); err == nil {
							success = ack.Success
						}
					}
					if incomingEnv.ID == env.ID || ack.EnvelopeID == env.ID || incomingEnv.ID == "" {
						if success {
							acked = true
						}
						break
					}
					c.handleFileCompleteAck(incomingEnv)
				} else {
					c.mu.Lock()
					handler := c.onCommand
					c.mu.Unlock()
					if handler != nil {
						if isPriorityCommand(incomingEnv.Type) {
							select {
							case c.prioritySem <- struct{}{}:
								go func(e protocol.Envelope) {
									defer func() { <-c.prioritySem }()
									handler(e)
								}(incomingEnv)
							default:
								log.Printf("Dropping %s command %s: %d priority handlers already in flight", incomingEnv.Type, incomingEnv.ID, maxConcurrentPriorityHandlers)
							}
						} else {
							select {
							case c.handlerSem <- struct{}{}:
								go func(e protocol.Envelope) {
									defer func() { <-c.handlerSem }()
									handler(e)
								}(incomingEnv)
							default:
							}
						}
					}
				}
			}
			_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
		}

		if !acked {
			log.Printf("Offline summary (id=%d) not acknowledged by server; retaining in offline queue", s.ID)
			break
		}

		// Delete from DB only after server ACK
		if err := db.New(c.config.DB).DeleteOfflineSummary(c.ctx, int64(s.ID)); err != nil {
			log.Printf("Failed to delete offline summary (id=%d): %v", s.ID, err)
		} else {
			log.Printf("Replayed and acknowledged offline summary id=%d", s.ID)
		}
	}
}

// isPriorityCommand returns true if the command type is an urgent cancellation
// that must be prioritized over saturated worker queues.
func isPriorityCommand(typ string) bool {
	return typ == protocol.TypeCancelJob || typ == protocol.TypeCancelAgentUpdate || typ == "job.cancel"
}
