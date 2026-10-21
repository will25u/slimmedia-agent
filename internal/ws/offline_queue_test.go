package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

var testUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func TestOfflineQueue_QueueWhenDisconnected(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ws_queue_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sqlDB, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer sqlDB.Close()

	client, err := NewClient(Config{DB: sqlDB, APIURL: "http://localhost:8082"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Stop()

	// 1. Enqueue FILE_COMPLETE envelope (file completion summary)
	workEnv := protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      "summary-1",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"file":"/media/movie.mkv","savings":1024000}`),
	}
	client.queueOffline(workEnv)

	// 2. Enqueue non-working envelope (e.g. heartbeat) - should be ignored by offline queue
	hbEnv := protocol.Envelope{
		Type:    "heartbeat",
		ID:      "hb-1",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"status":"IDLE"}`),
	}
	client.queueOffline(hbEnv)

	// Verify only FILE_COMPLETE was stored in SQLite
	summaries, err := db.New(sqlDB).GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries failed: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected exactly 1 offline summary queued, got %d", len(summaries))
	}

	var storedEnv protocol.Envelope
	if err := json.Unmarshal([]byte(summaries[0].Payload), &storedEnv); err != nil {
		t.Fatalf("failed to parse stored envelope: %v", err)
	}
	if storedEnv.ID != "summary-1" || storedEnv.Type != protocol.TypeFileComplete {
		t.Errorf("stored envelope mismatch: id=%s, type=%s", storedEnv.ID, storedEnv.Type)
	}
}

func TestOfflineQueue_ReplayOnReconnect(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ws_replay_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sqlDB, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer sqlDB.Close()

	// Pre-populate SQLite with 2 offline summaries
	q := db.New(sqlDB)
	env1 := protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      "env-1",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"step":1}`),
	}
	b1, _ := json.Marshal(env1)
	if err := q.InsertOfflineSummary(context.Background(), string(b1)); err != nil {
		t.Fatalf("failed to insert summary 1: %v", err)
	}

	env2 := protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      "env-2",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"step":2}`),
	}
	b2, _ := json.Marshal(env2)
	if err := q.InsertOfflineSummary(context.Background(), string(b2)); err != nil {
		t.Fatalf("failed to insert summary 2: %v", err)
	}

	// Spin up a mock WebSocket server
	var mu sync.Mutex
	var receivedEnvelopes []protocol.Envelope

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade error: %v", err)
			return
		}
		defer conn.Close()

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var env protocol.Envelope
			if err := json.Unmarshal(msg, &env); err == nil {
				mu.Lock()
				receivedEnvelopes = append(receivedEnvelopes, env)
				mu.Unlock()

				ackEnv := protocol.Envelope{
					Type: protocol.TypeFileCompleteAck,
					ID:   env.ID,
					TS:   time.Now(),
				}
				ackBytes, _ := json.Marshal(ackEnv)
				_ = conn.WriteMessage(websocket.TextMessage, ackBytes)
			}
		}
	}))
	defer server.Close()

	// Connect to mock server
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial mock ws server failed: %v", err)
	}
	defer clientConn.Close()

	client, err := NewClient(Config{DB: sqlDB, APIURL: "http://localhost:8082"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Stop()

	// Assign the active connection
	client.mu.Lock()
	client.conn = clientConn
	client.mu.Unlock()

	// Execute replay
	client.replayOfflineSummaries()

	// Wait briefly for mock server to receive messages
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	recCount := len(receivedEnvelopes)
	mu.Unlock()

	if recCount != 2 {
		t.Fatalf("expected mock server to receive 2 replayed envelopes, got %d", recCount)
	}

	// Verify SQLite queue was drained
	remaining, err := q.GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries error: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected 0 remaining summaries in sqlite after replay, got %d", len(remaining))
	}
}

func TestOfflineQueue_WithoutAckNotDeleted(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ws_noack_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sqlDB, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer sqlDB.Close()

	oldTimeout := offlineReplayAckTimeout
	offlineReplayAckTimeout = 150 * time.Millisecond
	defer func() { offlineReplayAckTimeout = oldTimeout }()

	q := db.New(sqlDB)
	env := protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      "env-noack",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"step":1}`),
	}
	b, _ := json.Marshal(env)
	if err := q.InsertOfflineSummary(context.Background(), string(b)); err != nil {
		t.Fatalf("failed to insert summary: %v", err)
	}

	// Spin up a mock WebSocket server that does NOT send ACK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
			// Purposefully do NOT send ACK
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial mock ws server failed: %v", err)
	}
	defer clientConn.Close()

	client, err := NewClient(Config{DB: sqlDB, APIURL: "http://localhost:8082"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Stop()

	client.mu.Lock()
	client.conn = clientConn
	client.mu.Unlock()

	// Execute replay with short read deadline simulation
	client.replayOfflineSummaries()

	// Verify SQLite queue STILL has the summary because it was NOT acknowledged
	remaining, err := q.GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries error: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected summary to be retained without ACK, got %d remaining", len(remaining))
	}
}

func TestOfflineQueue_AgentDeletionFlushesAndAcknowledges(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ws_del_flush_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sqlDB, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer sqlDB.Close()

	q := db.New(sqlDB)
	env := protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      "env-flush-on-del",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"step":1}`),
	}
	b, _ := json.Marshal(env)
	if err := q.InsertOfflineSummary(context.Background(), string(b)); err != nil {
		t.Fatalf("failed to insert summary: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var receivedEnv protocol.Envelope
			if err := json.Unmarshal(msg, &receivedEnv); err == nil {
				ackEnv := protocol.Envelope{
					Type: protocol.TypeFileCompleteAck,
					ID:   receivedEnv.ID,
					TS:   time.Now(),
				}
				ackBytes, _ := json.Marshal(ackEnv)
				_ = conn.WriteMessage(websocket.TextMessage, ackBytes)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial mock ws server failed: %v", err)
	}
	defer clientConn.Close()

	client, err := NewClient(Config{DB: sqlDB, APIURL: "http://localhost:8082"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Stop()

	client.mu.Lock()
	client.conn = clientConn
	client.mu.Unlock()

	// Calling handleAgentDeleted flushes pending offline summaries and waits for ACK before unpairing
	client.handleAgentDeleted()

	remaining, err := q.GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries error: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining summaries after deletion flush, got %d", len(remaining))
	}
}

func TestOfflineQueue_ConcurrentWritesAndReplayNoRace(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ws_concurr_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sqlDB, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer sqlDB.Close()

	q := db.New(sqlDB)
	for i := 0; i < 5; i++ {
		env := protocol.Envelope{
			Type:    protocol.TypeFileComplete,
			ID:      strings.Repeat("a", 10),
			TS:      time.Now(),
			Payload: json.RawMessage(`{"step":1}`),
		}
		b, _ := json.Marshal(env)
		if err := q.InsertOfflineSummary(context.Background(), string(b)); err != nil {
			t.Fatalf("failed to insert summary %d: %v", i, err)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var receivedEnv protocol.Envelope
			if err := json.Unmarshal(msg, &receivedEnv); err == nil {
				if receivedEnv.Type == protocol.TypeFileComplete {
					ackEnv := protocol.Envelope{
						Type: protocol.TypeFileCompleteAck,
						ID:   receivedEnv.ID,
						TS:   time.Now(),
					}
					ackBytes, _ := json.Marshal(ackEnv)
					_ = conn.WriteMessage(websocket.TextMessage, ackBytes)
				}
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial mock ws server failed: %v", err)
	}
	defer clientConn.Close()

	client, err := NewClient(Config{DB: sqlDB, APIURL: "http://localhost:8082"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Stop()

	client.mu.Lock()
	client.conn = clientConn
	client.mu.Unlock()

	go client.writeLoop()

	var wg sync.WaitGroup
	// Concurrently send messages via writeLoop
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				client.Send(protocol.Envelope{
					Type: "heartbeat",
					ID:   "hb",
					TS:   time.Now(),
				})
				time.Sleep(1 * time.Millisecond)
			}
		}(i)
	}

	// Concurrently run offline replay
	wg.Add(1)
	go func() {
		defer wg.Done()
		client.replayOfflineSummaries()
	}()

	wg.Wait()

	remaining, err := q.GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries error: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected all offline summaries to be replayed, remaining: %d", len(remaining))
	}
}

func TestOfflineQueue_NoConcurrentReadMessageOnActiveConnection(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ws_active_replay_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sqlDB, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer sqlDB.Close()

	oldTimeout := offlineReplayAckTimeout
	offlineReplayAckTimeout = 1 * time.Second
	defer func() { offlineReplayAckTimeout = oldTimeout }()

	q := db.New(sqlDB)
	for i := 1; i <= 3; i++ {
		env := protocol.Envelope{
			Type:    protocol.TypeFileComplete,
			ID:      fmt.Sprintf("active-env-%d", i),
			TS:      time.Now(),
			Payload: json.RawMessage(fmt.Sprintf(`{"step":%d}`, i)),
		}
		b, _ := json.Marshal(env)
		if err := q.InsertOfflineSummary(context.Background(), string(b)); err != nil {
			t.Fatalf("failed to insert summary: %v", err)
		}
	}

	cmdReceived := make(chan string, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var env protocol.Envelope
			if err := json.Unmarshal(msg, &env); err == nil {
				if env.Type == protocol.TypeFileComplete {
					ackPayload, _ := json.Marshal(protocol.FileCompleteAckPayload{
						EnvelopeID: env.ID,
						Success:    true,
					})
					ackEnv := protocol.Envelope{
						Type:    protocol.TypeFileCompleteAck,
						ID:      env.ID,
						TS:      time.Now(),
						Payload: ackPayload,
					}
					ackBytes, _ := json.Marshal(ackEnv)
					_ = conn.WriteMessage(websocket.TextMessage, ackBytes)

					// Concurrently send a command frame from server to agent
					// while replay is active to ensure the single reader loop receives it
					cmdEnv := protocol.Envelope{
						Type: "test.command",
						ID:   "cmd-" + env.ID,
						TS:   time.Now(),
					}
					cmdBytes, _ := json.Marshal(cmdEnv)
					_ = conn.WriteMessage(websocket.TextMessage, cmdBytes)
				}
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client, err := NewClient(Config{
		DB:     sqlDB,
		WSURL:  wsURL,
		APIURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Stop()

	client.SetCommandHandler(func(env protocol.Envelope) {
		cmdReceived <- env.ID
	})

	client.mu.Lock()
	client.token = "test-token"
	client.mu.Unlock()

	go client.maintainConnection()

	// Wait for queue to be drained
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		remaining, err := q.GetOfflineSummaries(context.Background())
		if err == nil && len(remaining) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	remaining, err := q.GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries failed: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining summaries after active replay, got %d", len(remaining))
	}

	// Verify that interleaved commands were delivered to command handler without reader conflicts
	for i := 1; i <= 3; i++ {
		select {
		case <-cmdReceived:
		case <-time.After(1 * time.Second):
			t.Fatalf("timed out waiting for interleaved command %d", i)
		}
	}
}

func TestOfflineQueue_TimeoutDoesNotPoisonConnection(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ws_poison_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sqlDB, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer sqlDB.Close()

	oldTimeout := offlineReplayAckTimeout
	offlineReplayAckTimeout = 200 * time.Millisecond
	defer func() { offlineReplayAckTimeout = oldTimeout }()

	q := db.New(sqlDB)
	env := protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      "env-no-ack",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"step":1}`),
	}
	b, _ := json.Marshal(env)
	if err := q.InsertOfflineSummary(context.Background(), string(b)); err != nil {
		t.Fatalf("failed to insert summary: %v", err)
	}

	probeReceived := make(chan struct{}, 1)
	var srvConnMu sync.Mutex
	var srvConn *websocket.Conn
	connEstablished := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		srvConnMu.Lock()
		srvConn = conn
		srvConnMu.Unlock()
		select {
		case connEstablished <- struct{}{}:
		default:
		}
		defer conn.Close()

		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
			// Purposefully DO NOT send ACK for FILE_COMPLETE
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client, err := NewClient(Config{
		DB:     sqlDB,
		WSURL:  wsURL,
		APIURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Stop()

	client.SetCommandHandler(func(env protocol.Envelope) {
		if env.Type == "probe.alive" {
			select {
			case probeReceived <- struct{}{}:
			default:
			}
		}
	})

	client.mu.Lock()
	client.token = "test-token"
	client.mu.Unlock()

	go client.maintainConnection()

	// Wait for connection to establish
	select {
	case <-connEstablished:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WS connection")
	}

	// Wait longer than offlineReplayAckTimeout for offline replay to time out waiting for ACK
	time.Sleep(350 * time.Millisecond)

	// Summary must be retained
	remaining, err := q.GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries failed: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected summary to be retained without ACK, got %d", len(remaining))
	}

	// Verify the connection was NOT poisoned and did not drop!
	if !client.IsConnected() {
		t.Fatal("connection dropped or entered reconnect loop after offline summary timeout")
	}

	// Send a probe frame from the server down the existing connection.
	// If the connection was poisoned with an i/o timeout, ReadMessage in maintainConnection
	// would have failed and closed the connection.
	srvConnMu.Lock()
	sc := srvConn
	srvConnMu.Unlock()
	if sc == nil {
		t.Fatal("server conn is nil")
	}

	probeEnv := protocol.Envelope{
		Type: "probe.alive",
		ID:   "probe-1",
		TS:   time.Now(),
	}
	pb, _ := json.Marshal(probeEnv)
	if err := sc.WriteMessage(websocket.TextMessage, pb); err != nil {
		t.Fatalf("failed to write probe: %v", err)
	}

	select {
	case <-probeReceived:
		// Success! Connection is intact and active reader received the probe!
	case <-time.After(2 * time.Second):
		t.Fatal("probe was not received by client: connection was poisoned or disconnected")
	}
}

func TestOfflineQueue_WriteLoopDrainsSendQueueOnShutdown(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ws_drain_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sqlDB, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer sqlDB.Close()

	client, err := NewClient(Config{DB: sqlDB, APIURL: "http://localhost:8082"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Enqueue terminal FILE_COMPLETE job evidence
	client.sendQueue <- protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      "complete-1",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"job_id":"job-1","status":"success"}`),
	}
	client.sendQueue <- protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      "complete-2",
		TS:      time.Now(),
		Payload: json.RawMessage(`{"job_id":"job-2","status":"success"}`),
	}

	// Trigger shutdown
	client.cancel()

	// Run writeLoop directly (it should see ctx.Done and drain sendQueue before returning)
	client.writeLoop()

	// Verify both summaries survived and are stored in SQLite
	summaries, err := db.New(sqlDB).GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries failed: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 offline summaries queued to SQLite on shutdown, got %d", len(summaries))
	}
}

func TestOfflineQueue_SizeLimitAndExpiration(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ws_bounds_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sqlDB, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer sqlDB.Close()

	// Shorten limit and TTL for test
	origLimit := maxOfflineSummaries
	origTTL := offlineSummaryTTL
	maxOfflineSummaries = 3
	offlineSummaryTTL = 50 * time.Millisecond
	defer func() {
		maxOfflineSummaries = origLimit
		offlineSummaryTTL = origTTL
	}()

	client, err := NewClient(Config{DB: sqlDB, APIURL: "http://localhost:8082"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Stop()

	// 1. Enqueue an expired envelope (timestamp in past)
	expiredEnv := protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      "expired-1",
		TS:      time.Now().Add(-100 * time.Millisecond),
		Payload: json.RawMessage(`{"job":"expired"}`),
	}
	client.queueOffline(expiredEnv)

	// 2. Enqueue 4 items when limit is 3
	for i := 1; i <= 4; i++ {
		env := protocol.Envelope{
			Type:    protocol.TypeFileComplete,
			ID:      fmt.Sprintf("item-%d", i),
			TS:      time.Now(),
			Payload: json.RawMessage(fmt.Sprintf(`{"job":%d}`, i)),
		}
		client.queueOffline(env)
	}

	// Verify that the queue does not exceed limit (3 items)
	q := db.New(sqlDB)
	summaries, err := q.GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries failed: %v", err)
	}
	if len(summaries) > 3 {
		t.Fatalf("queue size exceeded bound 3: got %d", len(summaries))
	}

	// Verify expired item is discarded during replay
	time.Sleep(60 * time.Millisecond) // Wait for TTL to expire
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial mock ws server failed: %v", err)
	}
	defer clientConn.Close()

	client.mu.Lock()
	client.conn = clientConn
	client.mu.Unlock()

	client.replayOfflineSummaries()

	// Expired messages should have been discarded and not left blocking
	remaining, err := q.GetOfflineSummaries(context.Background())
	if err != nil {
		t.Fatalf("GetOfflineSummaries failed: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected all expired summaries to be discarded, got %d remaining", len(remaining))
	}
}




