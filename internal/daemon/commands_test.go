package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// fakeJobCommands records the dispatch calls it receives in place of the
// JobSupervisor, so the dispatch test needs no runner, database, or
// websocket.
type fakeJobCommands struct {
	mu        sync.Mutex
	started   []protocol.StartJobPayload
	startRaw  []json.RawMessage
	cancelled []string
	paused    []string
	resumed   []string
	scheduled []*protocol.ScheduleConfig
	rates     []protocol.SetTelemetryRatePayload
}

func (f *fakeJobCommands) StartJob(_ context.Context, p protocol.StartJobPayload, raw json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, p)
	f.startRaw = append(f.startRaw, raw)
	return nil
}

func (f *fakeJobCommands) CancelJob(_ context.Context, jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, jobID)
}

func (f *fakeJobCommands) PauseJob(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = append(f.paused, jobID)
}

func (f *fakeJobCommands) ResumeJob(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, jobID)
}

func (f *fakeJobCommands) UpdateSchedule(sched *protocol.ScheduleConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scheduled = append(f.scheduled, sched)
}

func (f *fakeJobCommands) SetTelemetryRate(p protocol.SetTelemetryRatePayload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rates = append(f.rates, p)
}

// fakeUpdater records update commands in place of the Daemon's updater.
type fakeUpdater struct {
	mu        sync.Mutex
	started   []protocol.AgentUpdatePayload
	cancelled int
}

func (f *fakeUpdater) StartUpdate(p protocol.AgentUpdatePayload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, p)
}

func (f *fakeUpdater) CancelUpdate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled++
}

// dispatchFixture wires a commandDispatcher to recording fakes.
type dispatchFixture struct {
	jobs            *fakeJobCommands
	updater         *fakeUpdater
	scanMu          sync.Mutex
	scans           []string
	setLibMu        sync.Mutex
	setLibraryPaths []protocol.SetLibraryPathPayload
	events          []Event
}

func newDispatchFixture() *dispatchFixture {
	f := &dispatchFixture{
		jobs:    &fakeJobCommands{},
		updater: &fakeUpdater{},
	}
	return f
}

func (f *dispatchFixture) dispatcher() *commandDispatcher {
	return &commandDispatcher{
		ctx:     context.Background(),
		jobs:    func() jobCommands { return f.jobs },
		updater: f.updater,
		scanner: func(path, libID string) {
			f.scanMu.Lock()
			defer f.scanMu.Unlock()
			f.scans = append(f.scans, path)
		},
		setLibraryPath: func(p protocol.SetLibraryPathPayload) {
			f.setLibMu.Lock()
			defer f.setLibMu.Unlock()
			f.setLibraryPaths = append(f.setLibraryPaths, p)
		},
		emit: func(ev Event) { f.events = append(f.events, ev) },
	}
}

// noDispatch asserts that no module received a call.
func (f *dispatchFixture) noDispatch(t *testing.T) {
	t.Helper()
	if len(f.jobs.started) != 0 || len(f.jobs.cancelled) != 0 ||
		len(f.jobs.paused) != 0 || len(f.jobs.resumed) != 0 {
		t.Fatalf("job module received calls: %+v", f.jobs)
	}
	if len(f.updater.started) != 0 || f.updater.cancelled != 0 {
		t.Fatalf("updater received calls: %+v", f.updater)
	}
}

// sawErrorEvent asserts an error event whose message contains want.
func (f *dispatchFixture) sawErrorEvent(want string) bool {
	for _, ev := range f.events {
		if ev.Type == EventError && strings.Contains(ev.Message, want) {
			return true
		}
	}
	return false
}

func TestCommandDispatch(t *testing.T) {
	startPayload := protocol.StartJobPayload{
		JobID:       "job-1",
		InputPath:   "/media/movie.mkv",
		TargetCodec: "hevc",
	}
	startRaw, err := json.Marshal(startPayload)
	if err != nil {
		t.Fatalf("marshal start payload: %v", err)
	}
	updatePayload := protocol.AgentUpdatePayload{
		Version:      "1.2.3",
		Checksum:     "abc123",
		DownloadPath: "/downloads/agent",
	}
	updateRaw, err := json.Marshal(updatePayload)
	if err != nil {
		t.Fatalf("marshal update payload: %v", err)
	}
	controlRaw, err := json.Marshal(jobControlPayload{JobID: "job-9"})
	if err != nil {
		t.Fatalf("marshal control payload: %v", err)
	}

	cases := []struct {
		name string
		env  protocol.Envelope
		want func(t *testing.T, f *dispatchFixture)
	}{
		{
			name: "START_JOB reaches the supervisor with the raw payload",
			env:  protocol.Envelope{Type: protocol.TypeStartJob, Payload: startRaw},
			want: func(t *testing.T, f *dispatchFixture) {
				if len(f.jobs.started) != 1 {
					t.Fatalf("started jobs = %d, want 1", len(f.jobs.started))
				}
				if !reflect.DeepEqual(f.jobs.started[0], func() protocol.StartJobPayload { var p protocol.StartJobPayload; json.Unmarshal(startRaw, &p); return p }()) {
					t.Fatalf("dispatched payload = %+v, want %+v", f.jobs.started[0], startPayload)
				}
				if string(f.jobs.startRaw[0]) != string(startRaw) {
					t.Fatalf("dispatched raw payload = %s, want %s", f.jobs.startRaw[0], startRaw)
				}
			},
		},
		{
			name: "legacy job.start alias reaches the supervisor",
			env:  protocol.Envelope{Type: "job.start", Payload: startRaw},
			want: func(t *testing.T, f *dispatchFixture) {
				if len(f.jobs.started) != 1 {
					t.Fatalf("started jobs = %d, want 1", len(f.jobs.started))
				}
			},
		},
		{
			name: "CANCEL_JOB reaches the supervisor",
			env:  protocol.Envelope{Type: protocol.TypeCancelJob, Payload: controlRaw},
			want: func(t *testing.T, f *dispatchFixture) {
				if len(f.jobs.cancelled) != 1 || f.jobs.cancelled[0] != "job-9" {
					t.Fatalf("cancelled = %v, want [job-9]", f.jobs.cancelled)
				}
			},
		},
		{
			name: "PAUSE_JOB reaches the supervisor",
			env:  protocol.Envelope{Type: protocol.TypePauseJob, Payload: controlRaw},
			want: func(t *testing.T, f *dispatchFixture) {
				if len(f.jobs.paused) != 1 || f.jobs.paused[0] != "job-9" {
					t.Fatalf("paused = %v, want [job-9]", f.jobs.paused)
				}
			},
		},
		{
			name: "RESUME_JOB reaches the supervisor",
			env:  protocol.Envelope{Type: protocol.TypeResumeJob, Payload: controlRaw},
			want: func(t *testing.T, f *dispatchFixture) {
				if len(f.jobs.resumed) != 1 || f.jobs.resumed[0] != "job-9" {
					t.Fatalf("resumed = %v, want [job-9]", f.jobs.resumed)
				}
			},
		},
		{
			name: "AGENT_UPDATE reaches the updater",
			env:  protocol.Envelope{Type: protocol.TypeAgentUpdate, Payload: updateRaw},
			want: func(t *testing.T, f *dispatchFixture) {
				if len(f.updater.started) != 1 || !reflect.DeepEqual(f.updater.started[0], updatePayload) {
					t.Fatalf("updater started = %+v, want %+v", f.updater.started, updatePayload)
				}
			},
		},
		{
			name: "CANCEL_AGENT_UPDATE reaches the updater",
			env:  protocol.Envelope{Type: protocol.TypeCancelAgentUpdate},
			want: func(t *testing.T, f *dispatchFixture) {
				if f.updater.cancelled != 1 {
					t.Fatalf("updater cancels = %d, want 1", f.updater.cancelled)
				}
			},
		},
		{
			name: "UPDATE_SCHEDULE reaches the supervisor",
			env:  protocol.Envelope{Type: protocol.TypeUpdateSchedule, Payload: []byte(`{"quiet_hours_enabled":true}`)},
			want: func(t *testing.T, f *dispatchFixture) {
				if len(f.jobs.scheduled) != 1 || !f.jobs.scheduled[0].QuietHoursEnabled {
					t.Fatalf("scheduled = %v, want 1 enabled schedule", f.jobs.scheduled)
				}
			},
		},
		{
			name: "SET_TELEMETRY_RATE reaches the supervisor",
			env:  protocol.Envelope{Type: protocol.TypeSetTelemetryRate, Payload: []byte(`{"mode":"BACKGROUND","rate_ms":20000}`)},
			want: func(t *testing.T, f *dispatchFixture) {
				if len(f.jobs.rates) != 1 || f.jobs.rates[0].Mode != protocol.TelemetryModeBackground || f.jobs.rates[0].RateMS != 20000 {
					t.Fatalf("rates = %v, want 1 BACKGROUND rate with 20000ms", f.jobs.rates)
				}
			},
		},
		{
			name: "START_SCAN reaches the scanner",
			env:  protocol.Envelope{Type: protocol.TypeStartScan, Payload: []byte(`{"path":"/media/library"}`)},
			want: func(t *testing.T, f *dispatchFixture) {
				time.Sleep(50 * time.Millisecond)
				f.scanMu.Lock()
				defer f.scanMu.Unlock()
				if len(f.scans) != 1 || f.scans[0] != "/media/library" {
					t.Fatalf("scans = %v, want [/media/library]", f.scans)
				}
			},
		},
		{
			name: "SET_LIBRARY_PATH reaches the handler",
			env:  protocol.Envelope{Type: protocol.TypeSetLibraryPath, Payload: []byte(`{"library_id":"lib-456","local_path":"/storage/movies"}`)},
			want: func(t *testing.T, f *dispatchFixture) {
				f.setLibMu.Lock()
				defer f.setLibMu.Unlock()
				if len(f.setLibraryPaths) != 1 || f.setLibraryPaths[0].LibraryID != "lib-456" || f.setLibraryPaths[0].LocalPath != "/storage/movies" {
					t.Fatalf("setLibraryPaths = %v, want [{lib-456 /storage/movies}]", f.setLibraryPaths)
				}
			},
		},
		{
			name: "malformed SET_LIBRARY_PATH payload errors and dispatches nothing",
			env:  protocol.Envelope{Type: protocol.TypeSetLibraryPath, Payload: json.RawMessage("{")},
			want: func(t *testing.T, f *dispatchFixture) {
				f.noDispatch(t)
				if !f.sawErrorEvent("malformed SET_LIBRARY_PATH payload") {
					t.Fatalf("events = %+v, want malformed-payload error", f.events)
				}
			},
		},
		{
			name: "unknown command type errors and dispatches nothing",
			env:  protocol.Envelope{Type: "FROBNICATE", Payload: controlRaw},
			want: func(t *testing.T, f *dispatchFixture) {
				f.noDispatch(t)
				if !f.sawErrorEvent(`unknown command type "FROBNICATE"`) {
					t.Fatalf("events = %+v, want unknown-type error", f.events)
				}
			},
		},
		{
			name: "malformed START_JOB payload errors and dispatches nothing",
			env:  protocol.Envelope{Type: protocol.TypeStartJob, Payload: json.RawMessage("{")},
			want: func(t *testing.T, f *dispatchFixture) {
				f.noDispatch(t)
				if !f.sawErrorEvent("malformed START_JOB payload") {
					t.Fatalf("events = %+v, want malformed-payload error", f.events)
				}
			},
		},
		{
			name: "malformed CANCEL_JOB payload errors and dispatches nothing",
			env:  protocol.Envelope{Type: protocol.TypeCancelJob, Payload: json.RawMessage("{")},
			want: func(t *testing.T, f *dispatchFixture) {
				f.noDispatch(t)
				if !f.sawErrorEvent("malformed CANCEL_JOB payload") {
					t.Fatalf("events = %+v, want malformed-payload error", f.events)
				}
			},
		},
		{
			name: "malformed PAUSE_JOB payload errors and dispatches nothing",
			env:  protocol.Envelope{Type: protocol.TypePauseJob, Payload: json.RawMessage("{")},
			want: func(t *testing.T, f *dispatchFixture) {
				f.noDispatch(t)
				if !f.sawErrorEvent("malformed PAUSE_JOB payload") {
					t.Fatalf("events = %+v, want malformed-payload error", f.events)
				}
			},
		},
		{
			name: "malformed RESUME_JOB payload errors and dispatches nothing",
			env:  protocol.Envelope{Type: protocol.TypeResumeJob, Payload: json.RawMessage("{")},
			want: func(t *testing.T, f *dispatchFixture) {
				f.noDispatch(t)
				if !f.sawErrorEvent("malformed RESUME_JOB payload") {
					t.Fatalf("events = %+v, want malformed-payload error", f.events)
				}
			},
		},
		{
			name: "malformed AGENT_UPDATE payload errors and dispatches nothing",
			env:  protocol.Envelope{Type: protocol.TypeAgentUpdate, Payload: json.RawMessage("{")},
			want: func(t *testing.T, f *dispatchFixture) {
				f.noDispatch(t)
				if !f.sawErrorEvent("malformed AGENT_UPDATE payload") {
					t.Fatalf("events = %+v, want malformed-payload error", f.events)
				}
			},
		},
		{
			name: "deeply nested payload rejected by depth limit",
			env: protocol.Envelope{
				Type:    protocol.TypeStartJob,
				Payload: json.RawMessage(strings.Repeat(`{"a":`, 70) + `"b"` + strings.Repeat(`}`, 70)),
			},
			want: func(t *testing.T, f *dispatchFixture) {
				f.noDispatch(t)
				if !f.sawErrorEvent("malformed START_JOB payload") {
					t.Fatalf("events = %+v, want malformed-payload error", f.events)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDispatchFixture()
			f.dispatcher().handle(tc.env)
			tc.want(t, f)
		})
	}
}
