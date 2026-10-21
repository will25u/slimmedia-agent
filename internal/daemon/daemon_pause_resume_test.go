package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/scanner"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// writeSlowStubBinaries plants ffmpeg/ffprobe stubs where each ffmpeg
// invocation appends a nanosecond timestamp to startsFile and then sleeps,
// so a test can observe inter-chunk pauses. ffprobe reports a 610-second
// input so the runner plans three 300s chunks.
func writeSlowStubBinaries(t *testing.T, dataDir, startsFile, duration string) {
	t.Helper()
	ffmpegStub := `#!/bin/sh
encoders=0
is_seg=0
pat=""
for arg in "$@"; do
	case "$arg" in
		-encoders) encoders=1 ;;
		segment) is_seg=1 ;;
	esac
	pat="$arg"
done
if [ "$encoders" = "1" ]; then
	echo " V..... = Video"
	echo " DEV.LS h264_nvenc      NVIDIA NVENC H.264 encoder (codec h264)"
	echo " DEV.L. hevc_nvenc      NVIDIA NVENC HEVC encoder (codec hevc)"
	echo " V....D libx265         libx265 H.265 / HEVC encoder (codec hevc)"
	exit 0
fi
if [ "$is_seg" = "1" ]; then
	for i in 0 1 2; do
		idx=$(printf '%03d' $i)
		out=$(echo "$pat" | sed "s/%03d/$idx/")
		touch "$out"
	done
	exit 0
fi
date +%s%N >> "` + startsFile + `"
sleep 0.4
exit 0
`
	if err := os.WriteFile(filepath.Join(dataDir, "ffmpeg"), []byte(ffmpegStub), 0755); err != nil {
		t.Fatalf("write ffmpeg stub: %v", err)
	}
	ffprobeStub := fmt.Sprintf(`#!/bin/sh
last=""
for arg in "$@"; do
	if [ "$arg" = "-show_streams" ]; then
		cat <<'EOF'
{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"duration":"%s","r_frame_rate":"25/1","avg_frame_rate":"25/1","nb_frames":"15250"}],"format":{"duration":"%s","size":"1048576","bit_rate":"838860"}}
EOF
		exit 0
	fi
	last="$arg"
done
case "$last" in
*/chunk-*.mkv|*/split-*.mkv)
cat <<'EOF'
{"streams":[{"codec_type":"video"}]}
EOF
exit 0
;;
esac
exit 0
`, duration, duration)
	if err := os.WriteFile(filepath.Join(dataDir, "ffprobe"), []byte(ffprobeStub), 0755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
}

func stubStartCount(t *testing.T, startsFile string) int {
	t.Helper()
	data, err := os.ReadFile(startsFile)
	if err != nil {
		return 0
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}

// TestDaemonPauseJobHaltsChunkedTranscode is the regression test for
// "pause never pauses": a PAUSE_JOB arriving mid-chunk must halt the
// segmented runner at the next chunk boundary (PAUSED status), hold every
// further chunk until RESUME_JOB, then continue.
func TestDaemonPauseJobHaltsChunkedTranscode(t *testing.T) {
	srv := newFakeServer(t)

	mediaDir := t.TempDir()
	moviePath := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(moviePath, []byte("fake video"), 0644); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	dataDir := t.TempDir()
	startsFile := filepath.Join(dataDir, "starts.log")
	writeSlowStubBinaries(t, dataDir, startsFile, "610.000000")

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

	// Non-legacy (preset-style) payload: tuning fields force the segmented
	// runner, which is the path dashboard-dispatched jobs take.
	jobPayload, err := json.Marshal(protocol.StartJobPayload{
		JobID:         "pause-job-1",
		InputPath:     moviePath,
		TargetCodec:   "hevc",
		Container:     "mkv",
		EncoderEngine: "cpu",
		QualityTarget: 22,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	srv.sendCommand(t, protocol.Envelope{
		Type:    protocol.TypeStartJob,
		ID:      "cmd-start",
		TS:      time.Now(),
		Payload: jobPayload,
	})

	// Let chunk 0 get going, then request a pause mid-chunk.
	time.Sleep(150 * time.Millisecond)
	baseline := stubStartCount(t, startsFile)
	pausePayload, _ := json.Marshal(map[string]string{"job_id": "pause-job-1"})
	srv.sendCommand(t, protocol.Envelope{
		Type:    protocol.TypePauseJob,
		ID:      "cmd-pause",
		TS:      time.Now(),
		Payload: pausePayload,
	})

	// The runner must reach the pause loop at the chunk boundary and report
	// PAUSED.
	waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventStatus && ev.Status == "PAUSED"
	})

	// While paused, no further ffmpeg invocation may start.
	time.Sleep(600 * time.Millisecond)
	if n := stubStartCount(t, startsFile); n != baseline {
		t.Fatalf("expected no new ffmpeg invocations while paused (baseline %d), got %d", baseline, n)
	}

	// Resume: the runner must fire OnPause(false) (WORKING) and start chunk 1.
	srv.sendCommand(t, protocol.Envelope{
		Type:    protocol.TypeResumeJob,
		ID:      "cmd-resume",
		TS:      time.Now(),
		Payload: pausePayload,
	})
	waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventStatus && ev.Status == "WORKING"
	})

	deadline := time.Now().Add(5 * time.Second)
	for stubStartCount(t, startsFile) < baseline+1 {
		if time.Now().After(deadline) {
			t.Fatalf("chunk 1 never started after resume (starts=%d, baseline=%d)", stubStartCount(t, startsFile), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDaemonPauseJobLegacyPayload documents the second gap: a legacy
// (pre-tuning, no-preset) payload runs as a single ffmpeg pass and the pause
// flag is never consulted, so PAUSE_JOB has no effect. The daemon still
// advertises "pausing after chunk" — the fix must make pause effective for
// legacy jobs too (pause before the pass starts).
func TestDaemonPauseJobLegacyPayload(t *testing.T) {
	srv := newFakeServer(t)

	mediaDir := t.TempDir()
	moviePath := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(moviePath, []byte("fake video"), 0644); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	dataDir := t.TempDir()
	startsFile := filepath.Join(dataDir, "starts.log")
	writeSlowStubBinaries(t, dataDir, startsFile, "610.000000")

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
		JobID:       "pause-job-legacy",
		InputPath:   moviePath,
		TargetCodec: "hevc",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	srv.sendCommand(t, protocol.Envelope{
		Type:    protocol.TypeStartJob,
		ID:      "cmd-start",
		TS:      time.Now(),
		Payload: jobPayload,
	})

	time.Sleep(150 * time.Millisecond)
	pausePayload, _ := json.Marshal(map[string]string{"job_id": "pause-job-legacy"})
	srv.sendCommand(t, protocol.Envelope{
		Type:    protocol.TypePauseJob,
		ID:      "cmd-pause",
		TS:      time.Now(),
		Payload: pausePayload,
	})

	// The legacy single pass must still honor the pause: it cannot start the
	// encode while the pause flag is set. (Currently this is where the bug
	// bites — the flag is ignored entirely.)
	waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventStatus && ev.Status == "PAUSED"
	})
}
