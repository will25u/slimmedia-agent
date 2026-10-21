package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsConnForTest dials a test websocket server whose upgraded connection is
// handed to handler.
func wsConnForTest(t *testing.T, handler func(*websocket.Conn)) (*websocket.Conn, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handler(conn)
	}))
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial test server: %v", err)
	}
	return conn, func() { conn.Close(); srv.Close() }
}

// TestSetupWSConn_SilentServerTimesOut: with no traffic from the server the
// read must fail within wsPongWait instead of blocking forever on a dead peer.
func TestSetupWSConn_SilentServerTimesOut(t *testing.T) {
	old := wsPongWait
	wsPongWait = 500 * time.Millisecond
	defer func() { wsPongWait = old }()

	conn, cleanup := wsConnForTest(t, func(srv *websocket.Conn) {
		defer srv.Close()
		time.Sleep(2 * time.Second)
	})
	defer cleanup()

	setupWSConn(conn)
	start := time.Now()
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("read from a silent server must fail with the read deadline")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("read error = %v, want i/o timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("read took %v; read deadline not applied promptly", elapsed)
	}
}

// TestSetupWSConn_PongExtendsReadDeadline: a pong from the server resets the
// read deadline, so the connection survives past the initial window; when the
// server then goes silent, the extended deadline still fires.
func TestSetupWSConn_PongExtendsReadDeadline(t *testing.T) {
	old := wsPongWait
	wsPongWait = 500 * time.Millisecond
	defer func() { wsPongWait = old }()

	conn, cleanup := wsConnForTest(t, func(srv *websocket.Conn) {
		defer srv.Close()
		// Pong inside the initial window; then silence.
		time.Sleep(wsPongWait / 2)
		_ = srv.WriteMessage(websocket.PongMessage, nil)
		time.Sleep(3 * time.Second)
	})
	defer cleanup()

	setupWSConn(conn)
	start := time.Now()
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("read must eventually fail when the server goes silent")
	}
	elapsed := time.Since(start)
	if elapsed < wsPongWait+150*time.Millisecond {
		t.Fatalf("read failed after %v; pong did not extend the deadline past %v", elapsed, wsPongWait)
	}
}

// TestPingLoop_StopsOnClose: closing the per-connection stop channel must end
// the ping goroutine (cleanup on reconnect/shutdown).
func TestPingLoop_StopsOnClose(t *testing.T) {
	conn, cleanup := wsConnForTest(t, func(srv *websocket.Conn) {
		defer srv.Close()
		time.Sleep(300 * time.Millisecond)
	})
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{ctx: ctx}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		c.pingLoop(conn, stop)
		close(done)
	}()
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pingLoop did not stop after close(stop)")
	}
}
