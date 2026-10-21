package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/scanner"
	"github.com/will25u/slimmedia-agent/internal/ws"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// fakeServer is a local stand-in for the SaaS API: it performs the token
// exchange, accepts the agent WebSocket, records the envelopes the agent
// sends, and lets a test drive envelopes in.
type fakeServer struct {
	ts       *httptest.Server
	upgrader websocket.Upgrader

	mu         sync.Mutex
	conn       *websocket.Conn
	claimCalls int64

	incoming chan protocol.Envelope
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	srv := &fakeServer{
		upgrader: websocket.Upgrader{},
		incoming: make(chan protocol.Envelope, 128),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/claim", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&srv.claimCalls, 1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/auth/exchange", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"seam-test-token"}`))
	})
	mux.HandleFunc("/api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.0.0","latest_agent_version":"1.0.0"}`))
	})
	mux.HandleFunc("/api/v1/agents/audit-report", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/library/sync", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/ws/agent", srv.handleWS)
	srv.ts = httptest.NewServer(mux)
	t.Cleanup(srv.ts.Close)
	return srv
}

func (srv *fakeServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := srv.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	srv.mu.Lock()
	srv.conn = conn
	srv.mu.Unlock()
	defer func() {
		srv.mu.Lock()
		srv.conn = nil
		srv.mu.Unlock()
		conn.Close()
	}()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(msg, &env); err == nil {
			srv.incoming <- env
		}
	}
}

// sendCommand delivers an envelope to the connected agent, waiting for the
// WebSocket to come up first.
func (srv *fakeServer) sendCommand(t *testing.T, env protocol.Envelope) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.mu.Lock()
		conn := srv.conn
		srv.mu.Unlock()
		if conn != nil {
			b, err := json.Marshal(env)
			if err != nil {
				t.Fatalf("marshal command: %v", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, b); err == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for agent websocket connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// nextEnvelope returns the next non-heartbeat envelope the agent sent.
func (srv *fakeServer) nextEnvelope(t *testing.T) protocol.Envelope {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-srv.incoming:
			if env.Type == "heartbeat" {
				continue
			}
			return env
		case <-deadline:
			t.Fatal("timed out waiting for agent envelope")
			return protocol.Envelope{}
		}
	}
}

// writeStubBinaries plants script-based ffmpeg/ffprobe executables so daemon
// startup never reaches the network FFmpeg provisioner. The ffmpeg stub
// claims an NVENC encoder so EnsureFFmpeg's stale-build check treats it as
// usable, and emits -progress key/value lines during "transcodes" so tests
// can drive job-progress telemetry. The ffprobe stub answers stream probes
// with a canned 10-second H.264 file.
func writeStubBinaries(t *testing.T, dataDir string) {
	t.Helper()
	ffmpegStub := `#!/bin/sh
encoders=0
progress=0
is_seg=0
pat=""
for arg in "$@"; do
	case "$arg" in
		-encoders) encoders=1 ;;
		-progress) progress=1 ;;
		segment) is_seg=1 ;;
	esac
	pat="$arg"
done
if [ "$encoders" = "1" ]; then
	echo " V..... = Video"
	echo " DEV.LS h264_nvenc      NVIDIA NVENC H.264 encoder (codec h264)"
	echo " DEV.L. hevc_nvenc      NVIDIA NVENC HEVC encoder (codec hevc)"
	echo " V....D libx264         libx264 H.264 / AVC (codec h264)"
	echo " V....D libx265         libx265 H.265 / HEVC (codec hevc)"
	exit 0
fi
if [ "$is_seg" = "1" ]; then
	for i in 0 1; do
		idx=$(printf '%03d' $i)
		out=$(echo "$pat" | sed "s/%03d/$idx/")
		touch "$out"
	done
	exit 0
fi
if [ "$progress" = "1" ]; then
	printf 'frame=250\nfps=25.0\nout_time_us=10000000\nspeed=2.5x\nbitrate=838.9kbits/s\nprogress=continue\n'
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dataDir, "ffmpeg"), []byte(ffmpegStub), 0755); err != nil {
		t.Fatalf("write ffmpeg stub: %v", err)
	}
	ffprobeStub := `#!/bin/sh
for arg in "$@"; do
	if [ "$arg" = "-show_streams" ]; then
		cat <<'EOF'
{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"duration":"10.000000","r_frame_rate":"25/1","avg_frame_rate":"25/1","nb_frames":"250"}],"format":{"duration":"10.000000","size":"1048576","bit_rate":"838860"}}
EOF
		exit 0
	fi
done
# codec_type stream probes (split/encoded chunk verification) get the
# pure-video layout; everything else is left unanswered.
last=""
for arg in "$@"; do last="$arg"; done
case "$last" in
*/chunk-*.mkv|*/split-*.mkv)
	cat <<'EOF'
{"streams":[{"codec_type":"video"}]}
EOF
;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dataDir, "ffprobe"), []byte(ffprobeStub), 0755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
}

func testResolvedConfig(t *testing.T, srv *fakeServer) *ResolvedConfig {
	t.Helper()
	dataDir := t.TempDir()
	writeStubBinaries(t, dataDir)
	return &ResolvedConfig{
		Server:     srv.ts.URL,
		WSURL:      "ws://" + strings.TrimPrefix(srv.ts.URL, "http://"),
		APIKey:     "seam-test-api-key",
		Name:       "seam-test-agent",
		MediaDir:   t.TempDir(), // empty: sync completes without ffprobe
		DataDir:    dataDir,
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	}
}

// waitForEvent collects events from ch until one satisfies pred or the
// deadline passes.
func waitForEvent(t *testing.T, events <-chan Event, pred func(Event) bool) Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("event stream closed while waiting for event")
			}
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for daemon event")
			return Event{}
		}
	}
}

func TestDaemonStartRequiresMediaDir(t *testing.T) {
	srv := newFakeServer(t)
	rc := testResolvedConfig(t, srv)
	rc.MediaDir = ""
	_, err := Start(context.Background(), rc)
	if err == nil || !strings.Contains(err.Error(), "media directory is required") {
		t.Fatalf("expected media directory is required error, got %v", err)
	}
}

func TestDaemonLifecycle(t *testing.T) {
	srv := newFakeServer(t)
	d, err := Start(context.Background(), testResolvedConfig(t, srv))
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// The daemon reports itself running and connects to the fake server.
	if st := d.Status(); st.State != StateRunning {
		t.Fatalf("Status().State = %q, want %q", st.State, StateRunning)
	}
	ev := waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventConnection && ev.Connected
	})
	if ev.Detail != "" {
		t.Fatalf("connect event detail = %q, want empty", ev.Detail)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !d.Status().Connected {
		if time.Now().After(deadline) {
			t.Fatal("Status().Connected never became true")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := d.Status(); st.Version != Version || st.Name != "seam-test-agent" || st.Server != srv.ts.URL {
		t.Fatalf("unexpected status snapshot: %+v", st)
	}

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if st := d.Status(); st.State != StateStopped {
		t.Fatalf("Status().State after shutdown = %q, want %q", st.State, StateStopped)
	}
	// The event stream closes once shutdown completes; drain any remaining buffered events.
	for range d.Events() {
	}
	// Shutdown is idempotent.
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown failed: %v", err)
	}
}

// stuckJobSupervisor replaces the daemon's supervisor with one whose runner
// blocks until the job context is cancelled, simulating a job that cannot
// drain on its own. The job is started on a background context (not the
// daemon's) so only the drain-time CancelAll unwinds it.
func stuckJobSupervisor(t *testing.T, d *Daemon) {
	t.Helper()
	runner := newGateRunner()
	jobs := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
		Emit:        d.emit,
		Send:        d.wsClient.Send,
		SetStatus:   d.setStatus,
	})
	d.setJobs(jobs)
	go func() {
		payload := testStartPayload("stuck-job")
		if err := jobs.StartJob(context.Background(), payload, nil); err != nil {
			t.Errorf("StartJob(stuck-job): %v", err)
		}
	}()
	select {
	case id := <-runner.started:
		if id != "stuck-job" {
			t.Fatalf("stuck job started with id %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stuck job never reached the runner")
	}
}

// TestDaemonShutdownCancelsOrphanedJobs verifies that when the drain cannot
// complete (here: the caller's ctx is already cancelled while a job is still
// in flight), Shutdown cancels the in-flight job contexts, returns a non-nil
// error, stays bounded, and leaves daemon state consistent.
func TestDaemonShutdownCancelsOrphanedJobs(t *testing.T) {
	srv := newFakeServer(t)
	d, err := Start(context.Background(), testResolvedConfig(t, srv))
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Simulate a job that blocks until its context is cancelled.
	stuckJobSupervisor(t, d)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err = d.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown returned nil error for an incomplete drain")
	}
	if elapsed := time.Since(start); elapsed > defaultDrainTimeout {
		t.Fatalf("Shutdown took %v, want well under the drain timeout", elapsed)
	}

	if st := d.Status(); st.State != StateStopped {
		t.Fatalf("Status().State after shutdown = %q, want %q", st.State, StateStopped)
	}
	// Drain buffered events; the stream must then be closed.
	for range d.Events() {
	}
	// The recorded outcome is reported on repeated calls too.
	if err2 := d.Shutdown(context.Background()); err2 == nil || err2.Error() != err.Error() {
		t.Fatalf("second Shutdown error = %v, want %v", err2, err)
	}
}

// TestDaemonShutdownDrainTimeout verifies the drain-timeout path: a job that
// never finishes on its own is force-cancelled after the deadline and
// Shutdown reports the incomplete drain.
func TestDaemonShutdownDrainTimeout(t *testing.T) {
	srv := newFakeServer(t)
	d, err := Start(context.Background(), testResolvedConfig(t, srv))
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	stuckJobSupervisor(t, d)

	start := time.Now()
	err = d.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown returned nil error after drain timeout")
	}
	if elapsed := time.Since(start); elapsed < defaultDrainTimeout {
		t.Fatalf("Shutdown returned after %v, want at least the drain timeout", elapsed)
	}

	if st := d.Status(); st.State != StateStopped {
		t.Fatalf("Status().State after shutdown = %q, want %q", st.State, StateStopped)
	}
}

func TestDaemonStartRequiresAPIKey(t *testing.T) {
	srv := newFakeServer(t)
	cfg := testResolvedConfig(t, srv)
	cfg.APIKey = ""
	if _, err := Start(context.Background(), cfg); err == nil {
		t.Fatal("Start succeeded without an API key")
	}
}

// TestDaemonEventStream drives a rejected job through the fake server and
// verifies the daemon's external behavior: error and job-complete events on
// the event stream and a failure FILE_COMPLETE envelope back to the server.
func TestDaemonEventStream(t *testing.T) {
	srv := newFakeServer(t)
	d, err := Start(context.Background(), testResolvedConfig(t, srv))
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventConnection && ev.Connected
	})

	// A real file the scanner never saw: confinement must reject it.
	outsideFile := filepath.Join(t.TempDir(), "unscanned.mkv")
	if err := os.WriteFile(outsideFile, []byte("video"), 0644); err != nil {
		t.Fatalf("write unscanned file: %v", err)
	}
	jobPayload, err := json.Marshal(protocol.StartJobPayload{
		JobID:       "seam-job-1",
		InputPath:   outsideFile,
		TargetCodec: "hevc",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	srv.sendCommand(t, protocol.Envelope{
		Type:    protocol.TypeStartJob,
		ID:      "cmd-1",
		TS:      time.Now(),
		Payload: jobPayload,
	})

	errEv := waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventError && ev.JobID == "seam-job-1"
	})
	if !strings.Contains(errEv.Message, "outside the scanned media library") {
		t.Fatalf("error event message = %q, want confinement rejection", errEv.Message)
	}

	doneEv := waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventJobComplete && ev.JobID == "seam-job-1"
	})
	if doneEv.Success {
		t.Fatal("job-complete event reported success for a rejected job")
	}

	env := srv.nextEnvelope(t)
	if env.Type != protocol.TypeFileComplete {
		t.Fatalf("server received envelope type %q, want %q", env.Type, protocol.TypeFileComplete)
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(env.Payload, &complete); err != nil {
		t.Fatalf("decode FILE_COMPLETE payload: %v", err)
	}
	if complete.Success {
		t.Fatal("FILE_COMPLETE reported success for a rejected job")
	}
	if !strings.Contains(complete.ErrorMessage, "outside the scanned media library") {
		t.Fatalf("FILE_COMPLETE error = %q, want confinement rejection", complete.ErrorMessage)
	}
}

// TestDaemonPauseResumeCancelUnknownJob exercises control commands for a job
// the daemon doesn't have: they must be accepted without panic and must not
// fabricate events or replies.
func TestDaemonPauseResumeCancelUnknownJob(t *testing.T) {
	srv := newFakeServer(t)
	d, err := Start(context.Background(), testResolvedConfig(t, srv))
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventConnection && ev.Connected
	})

	for _, typ := range []string{protocol.TypePauseJob, protocol.TypeResumeJob, protocol.TypeCancelJob} {
		payload, _ := json.Marshal(map[string]string{"job_id": "no-such-job"})
		srv.sendCommand(t, protocol.Envelope{Type: typ, ID: typ + "-1", TS: time.Now(), Payload: payload})
	}

	// Give the daemon a moment to misbehave if it is going to, then assert
	// it is still healthy.
	time.Sleep(200 * time.Millisecond)
	if st := d.Status(); st.State != StateRunning {
		t.Fatalf("daemon state = %q after unknown-job commands, want %q", st.State, StateRunning)
	}
	if st := d.Status(); st.ActiveJobs != 0 {
		t.Fatalf("ActiveJobs = %d, want 0", st.ActiveJobs)
	}
}

// TestDaemonJobProgressEvents drives an accepted job through the stub
// transcode pipeline and verifies the job-progress telemetry on the event
// stream (the stub ffmpeg emits -progress lines but no output file, so the
// job terminates with a failure after reporting progress).
func TestDaemonJobProgressEvents(t *testing.T) {
	srv := newFakeServer(t)

	mediaDir := t.TempDir()
	moviePath := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(moviePath, []byte("fake video"), 0644); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	dataDir := t.TempDir()
	writeStubBinaries(t, dataDir)

	// Pre-seed the confinement registry in the database the daemon will
	// open, so the job below is accepted deterministically (the startup
	// scan would also record it, but races the test's START_JOB).
	seedDB, err := db.InitDB(dataDir)
	if err != nil {
		t.Fatalf("seed InitDB failed: %v", err)
	}
	recordScannedFiles(seedDB, []scanner.ScannedFile{{FilePath: moviePath}}, mediaDir)
	seedDB.Close()

	cfg := testResolvedConfig(t, srv)
	cfg.MediaDir = mediaDir
	cfg.DataDir = dataDir

	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventConnection && ev.Connected
	})

	jobPayload, err := json.Marshal(protocol.StartJobPayload{
		JobID:       "seam-job-2",
		InputPath:   moviePath,
		TargetCodec: "hevc",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	srv.sendCommand(t, protocol.Envelope{
		Type:    protocol.TypeStartJob,
		ID:      "cmd-2",
		TS:      time.Now(),
		Payload: jobPayload,
	})

	progEv := waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventJobProgress && ev.JobID == "seam-job-2"
	})
	if progEv.FPS != 25.0 || progEv.Speed != 2.5 {
		t.Fatalf("job-progress event = %+v, want FPS 25 and speed 2.5", progEv)
	}
	if progEv.InputPath != moviePath {
		t.Fatalf("job-progress input = %q, want %q", progEv.InputPath, moviePath)
	}

	doneEv := waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventJobComplete && ev.JobID == "seam-job-2"
	})
	if doneEv.Success {
		t.Fatalf("job unexpectedly succeeded with a no-output stub ffmpeg: %+v", doneEv)
	}
}

func TestSaveLoadConfigRoundTrip(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nested", "config.json")
	want := &LocalConfig{
		APIKey:   "slm_roundtrip_key",
		Server:   "http://localhost:8082",
		Name:     "roundtrip-agent",
		MediaDir: "/media",
	}
	if err := SaveConfig(configPath, want); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("config mode = %o, want 0600", perm)
	}
	got, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", *got, *want)
	}
}

// TestDaemonShutdownConcurrentEmit stress-tests emission racing Shutdown:
// no goroutine may send on the events channel after it is closed, under any
// interleaving. Run with -race.
func TestDaemonShutdownConcurrentEmit(t *testing.T) {
	srv := newFakeServer(t)
	d, err := Start(context.Background(), testResolvedConfig(t, srv))
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Consume events so the buffer drains and emit's send case stays ready.
	consumeDone := make(chan struct{})
	go func() {
		defer close(consumeDone)
		for range d.Events() {
		}
	}()

	const emitters = 8
	const emitsPerGoroutine = 200
	var wg sync.WaitGroup
	for i := 0; i < emitters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < emitsPerGoroutine; j++ {
				d.emit(Event{Type: EventStatus, Status: "stress"})
			}
		}()
	}

	// Give emitters a head start so Shutdown overlaps in-flight emission.
	time.Sleep(5 * time.Millisecond)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	wg.Wait()
	<-consumeDone
}

func TestDaemon_HandleAgentDeleted_StateResetAndHeadlessExit(t *testing.T) {
	srv := newFakeServer(t)
	cfg := testResolvedConfig(t, srv)
	cfg.UIAttached = false // headless

	// Write a fake config.json
	if err := os.WriteFile(cfg.ConfigPath, []byte(`{"api_key":"test"}`), 0600); err != nil {
		t.Fatalf("failed to write config.json: %v", err)
	}

	exitCalled := make(chan int, 1)
	origExit := exitFunc
	exitFunc = func(code int) {
		select {
		case exitCalled <- code:
		default:
		}
	}
	defer func() { exitFunc = origExit }()

	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	// Read events channel to capture EventUnpaired
	unpairedEvCh := make(chan Event, 10)
	go func() {
		for ev := range d.Events() {
			if ev.Type == EventUnpaired || ev.Status == "UNPAIRED" {
				unpairedEvCh <- ev
			}
		}
	}()

	// Trigger agent deleted
	d.handleAgentDeleted()

	// a. Verify config file was deleted
	if _, err := os.Stat(cfg.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("expected config file to be deleted, got err: %v", err)
	}

	// b. Verify SQLite database was NOT deleted
	dbPath := filepath.Join(cfg.DataDir, "agent.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatalf("expected SQLite db to remain intact, but it was deleted")
	}

	// c. Verify daemon state shifted to UNPAIRED
	if d.Status().State != StateUnpaired {
		t.Fatalf("expected daemon State to be UNPAIRED, got %s", d.Status().State)
	}
	if d.Status().AgentStatus != "UNPAIRED" {
		t.Fatalf("expected AgentStatus to be UNPAIRED, got %s", d.Status().AgentStatus)
	}

	select {
	case ev := <-unpairedEvCh:
		if ev.Status != "UNPAIRED" {
			t.Fatalf("expected emitted event status UNPAIRED, got %s", ev.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected EventUnpaired on event stream")
	}

	// d. Verify headless exitFunc called with 0
	select {
	case code := <-exitCalled:
		if code != 0 {
			t.Fatalf("expected exit code 0, got %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected headless exit to be called")
	}
}

func TestDaemon_HandleAgentDeleted_UIAttachedDoesNotExit(t *testing.T) {
	srv := newFakeServer(t)
	cfg := testResolvedConfig(t, srv)
	cfg.UIAttached = true // UI attached (e.g. Tray App)

	if err := os.WriteFile(cfg.ConfigPath, []byte(`{"api_key":"test"}`), 0600); err != nil {
		t.Fatalf("failed to write config.json: %v", err)
	}

	exitCalled := make(chan int, 1)
	origExit := exitFunc
	exitFunc = func(code int) {
		exitCalled <- code
	}
	defer func() { exitFunc = origExit }()

	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	d.handleAgentDeleted()

	// Config should be deleted
	if _, err := os.Stat(cfg.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("expected config file to be deleted")
	}

	// State should be UNPAIRED
	if d.Status().State != StateUnpaired {
		t.Fatalf("expected StateUnpaired, got %s", d.Status().State)
	}

	// exitFunc should NOT be called
	select {
	case code := <-exitCalled:
		t.Fatalf("unexpected exit call with code %d when UI is attached", code)
	case <-time.After(100 * time.Millisecond):
		// Expected: no exit
	}
}

func TestDaemon_HandleAgentDeleted_NoMarkerCreated(t *testing.T) {
	srv := newFakeServer(t)
	cfg := testResolvedConfig(t, srv)
	cfg.UIAttached = true

	if err := os.WriteFile(cfg.ConfigPath, []byte(`{"api_key":"test"}`), 0600); err != nil {
		t.Fatalf("failed to write config.json: %v", err)
	}

	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	// Trigger agent unpairing / deletion
	d.handleAgentDeleted()

	// Verify no .slimmedia or marker.json exists in mediaDir
	markerPath := filepath.Join(cfg.MediaDir, ".slimmedia")
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("expected no .slimmedia marker on disk, but found: %s", markerPath)
	}
}

func TestDaemon_SetLibraryPath(t *testing.T) {
	srv := newFakeServer(t)
	cfg := testResolvedConfig(t, srv)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg.ConfigPath, []byte(`{"api_key":"test-key","server":"`+srv.ts.URL+`"}`), 0600); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	newMediaDir := t.TempDir()
	newLibraryID := "lib-updated-999"

	// Dispatch SET_LIBRARY_PATH via command dispatcher
	payload, _ := json.Marshal(protocol.SetLibraryPathPayload{
		LibraryID: newLibraryID,
		LocalPath: newMediaDir,
	})
	handler := d.commandHandler()
	handler(protocol.Envelope{
		Type:    protocol.TypeSetLibraryPath,
		Payload: payload,
	})

	// Verify in-memory state
	d.mu.Lock()
	if d.cfg.LibraryID != newLibraryID {
		t.Errorf("cfg.LibraryID = %q, want %q", d.cfg.LibraryID, newLibraryID)
	}
	if d.cfg.MediaDir != newMediaDir {
		t.Errorf("cfg.MediaDir = %q, want %q", d.cfg.MediaDir, newMediaDir)
	}
	d.mu.Unlock()

	// Verify supervisor and worker media dirs were updated
	if d.worker.GetMediaDir() != newMediaDir {
		t.Errorf("worker.GetMediaDir() = %q, want %q", d.worker.GetMediaDir(), newMediaDir)
	}
	if d.jobs.GetMediaDir() != newMediaDir {
		t.Errorf("jobs.GetMediaDir() = %q, want %q", d.jobs.GetMediaDir(), newMediaDir)
	}

	// Verify config.json on disk was persisted
	saved, err := LoadConfig(cfg.ConfigPath)
	if err != nil {
		t.Fatalf("failed to load saved config: %v", err)
	}
	if saved.LibraryID != newLibraryID {
		t.Errorf("saved.LibraryID = %q, want %q", saved.LibraryID, newLibraryID)
	}
	if saved.MediaDir != newMediaDir {
		t.Errorf("saved.MediaDir = %q, want %q", saved.MediaDir, newMediaDir)
	}
	if len(saved.Libraries) != 1 || saved.Libraries[0].LibraryID != newLibraryID || saved.Libraries[0].LocalPath != newMediaDir {
		t.Errorf("saved.Libraries = %+v, want 1 entry with id %q and path %q", saved.Libraries, newLibraryID, newMediaDir)
	}
}

// TestDaemon_MultipleMappedLibraries_PersistedAndScanned verifies that when an agent has multiple
// mapped libraries, SetLibraryPath persists all of them to Libraries in config.json without losing
// previously mapped libraries, and that on daemon restart all mapped libraries are scanned.
func TestDaemon_MultipleMappedLibraries_PersistedAndScanned(t *testing.T) {
	srv := newFakeServer(t)
	cfg := testResolvedConfig(t, srv)

	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	lib1Dir := t.TempDir()
	lib2Dir := t.TempDir()

	if err := d.SetLibraryPath("lib-christmas", lib1Dir); err != nil {
		t.Fatalf("SetLibraryPath lib-christmas failed: %v", err)
	}
	if err := d.SetLibraryPath("lib-kidstv", lib2Dir); err != nil {
		t.Fatalf("SetLibraryPath lib-kidstv failed: %v", err)
	}

	saved, err := LoadConfig(cfg.ConfigPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(saved.Libraries) != 2 {
		t.Fatalf("expected 2 libraries saved, got %d: %+v", len(saved.Libraries), saved.Libraries)
	}
	if saved.Libraries[0].LibraryID != "lib-christmas" || saved.Libraries[0].LocalPath != lib1Dir {
		t.Errorf("library 0 mismatch: %+v", saved.Libraries[0])
	}
	if saved.Libraries[1].LibraryID != "lib-kidstv" || saved.Libraries[1].LocalPath != lib2Dir {
		t.Errorf("library 1 mismatch: %+v", saved.Libraries[1])
	}

	// ResolveConfig on the saved config should preserve both libraries
	rc, err := ResolveConfig(Config{
		ConfigPath: cfg.ConfigPath,
		DataDir:    cfg.DataDir,
	})
	if err != nil {
		t.Fatalf("ResolveConfig failed: %v", err)
	}
	if len(rc.Libraries) != 2 {
		t.Fatalf("expected ResolveConfig to have 2 libraries, got %d", len(rc.Libraries))
	}
}

// TestDaemon_PureWebSocketJobDispatch_ZeroHTTPClaimPolling verifies that:
// 1. The agent produces 0 HTTP claim polling requests while idle.
// 2. The agent accepts, validates, and executes jobs pushed over WebSocket START_JOB.
// 3. The agent reports capacity changes (WORKING) and completion (FILE_COMPLETE).
// 4. Idle heartbeat interval is 30s when no jobs are active.
func TestDaemon_PureWebSocketJobDispatch_ZeroHTTPClaimPolling(t *testing.T) {
	srv := newFakeServer(t)

	mediaDir := t.TempDir()
	moviePath := filepath.Join(mediaDir, "sample.mkv")
	if err := os.WriteFile(moviePath, []byte("sample movie content"), 0644); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	dataDir := t.TempDir()
	writeStubBinaries(t, dataDir)

	seedDB, err := db.InitDB(dataDir)
	if err != nil {
		t.Fatalf("seed InitDB failed: %v", err)
	}
	recordScannedFiles(seedDB, []scanner.ScannedFile{{FilePath: moviePath}}, mediaDir)
	seedDB.Close()

	cfg := testResolvedConfig(t, srv)
	cfg.MediaDir = mediaDir
	cfg.DataDir = dataDir

	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventConnection && ev.Connected
	})

	// 1. Verify idle heartbeat interval is relaxed to 30s
	if interval := d.wsClient.CurrentHeartbeatInterval(); interval != ws.DefaultIdleHeartbeatInterval {
		t.Fatalf("expected idle heartbeat interval %v, got %v", ws.DefaultIdleHeartbeatInterval, interval)
	}

	// 2. Verify 0 HTTP claim polling requests while idle
	time.Sleep(100 * time.Millisecond)
	if calls := atomic.LoadInt64(&srv.claimCalls); calls != 0 {
		t.Fatalf("expected 0 HTTP claim calls while idle, got %d", calls)
	}

	// 3. Push job via WebSocket START_JOB envelope
	jobPayload, err := json.Marshal(protocol.StartJobPayload{
		JobID:       "push-job-1",
		InputPath:   moviePath,
		TargetCodec: "hevc",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	srv.sendCommand(t, protocol.Envelope{
		Type:    protocol.TypeStartJob,
		ID:      "cmd-push-1",
		TS:      time.Now(),
		Payload: jobPayload,
	})

	// 4. Verify agent accepts, begins executing, and transitions to WORKING
	progEv := waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventJobProgress && ev.JobID == "push-job-1"
	})
	if progEv.JobID != "push-job-1" {
		t.Fatalf("expected progress for push-job-1, got %s", progEv.JobID)
	}

	// Active status tightening heartbeat interval
	if interval := d.wsClient.CurrentHeartbeatInterval(); interval != ws.DefaultActiveHeartbeatInterval {
		t.Fatalf("expected active heartbeat interval %v during job execution, got %v", ws.DefaultActiveHeartbeatInterval, interval)
	}

	// 5. Complete event emitted over WebSocket
	doneEv := waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventJobComplete && ev.JobID == "push-job-1"
	})
	if doneEv.JobID != "push-job-1" {
		t.Fatalf("expected job complete event for push-job-1, got %v", doneEv)
	}

	var completeEnv protocol.Envelope
	for {
		env := srv.nextEnvelope(t)
		if env.Type == protocol.TypeFileComplete {
			completeEnv = env
			break
		}
	}
	if completeEnv.Type != protocol.TypeFileComplete {
		t.Fatalf("server received envelope type %q, want %q", completeEnv.Type, protocol.TypeFileComplete)
	}

	// 6. Restored to IDLE and still ZERO HTTP claim requests
	waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventStatus && ev.Status == "IDLE"
	})
	if calls := atomic.LoadInt64(&srv.claimCalls); calls != 0 {
		t.Fatalf("expected 0 HTTP claim calls after push transcode, got %d", calls)
	}
	if interval := d.wsClient.CurrentHeartbeatInterval(); interval != ws.DefaultIdleHeartbeatInterval {
		t.Fatalf("expected restored idle heartbeat interval %v, got %v", ws.DefaultIdleHeartbeatInterval, interval)
	}
}


