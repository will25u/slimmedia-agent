package ffmpeg

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestMeasureVMAFAgainstRealFFmpeg synthesizes two short clips, one identical
// to the reference and one visually different, and checks the VMAF spot
// check scores them ~100 and distinctly lower respectively.
func TestMeasureVMAFAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, _ := repoBinFFmpeg(t)
	tmp := t.TempDir()

	synthesize := func(name, pattern string) string {
		p := filepath.Join(tmp, name)
		cmd := exec.Command(ffmpegPath,
			"-y", "-v", "error",
			"-f", "lavfi", "-i", pattern,
			"-t", "4",
			"-pix_fmt", "yuv420p",
			p)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("synthesizing %s: %v: %s", name, err, out)
		}
		return p
	}

	reference := synthesize("ref.mp4", "testsrc2=size=320x240:rate=25")
	identical := synthesize("identical.mp4", "testsrc2=size=320x240:rate=25")
	different := synthesize("different.mp4", "testsrc=size=320x240:rate=25")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	same, err := MeasureVMAF(ctx, ffmpegPath, reference, identical, 0.5, 2)
	if err != nil {
		t.Fatalf("MeasureVMAF on identical outputs: %v", err)
	}
	if same < 95.0 {
		t.Errorf("identical outputs scored %f, want >= 95.0", same)
	}

	diff, err := MeasureVMAF(ctx, ffmpegPath, reference, different, 0.5, 2)
	if err != nil {
		t.Fatalf("MeasureVMAF on different outputs: %v", err)
	}
	if diff >= same {
		t.Errorf("different content scored %f, want clearly below identical %f", diff, same)
	}
}

// TestMeasureVMAFUnreadableInput verifies an unreadable input produces an
// error that the orchestrator can treat as a skip.
func TestMeasureVMAFUnreadableInput(t *testing.T) {
	ffmpegPath, _ := repoBinFFmpeg(t)
	_, err := MeasureVMAF(context.Background(), ffmpegPath,
		filepath.Join(t.TempDir(), "does-not-exist.mkv"),
		filepath.Join(t.TempDir(), "also-missing.mkv"),
		0, 1)
	if err == nil {
		t.Fatal("expected an error for unreadable inputs")
	}
}

func TestQualityFloorForPreset(t *testing.T) {
	if got := QualityFloorForPreset("The Archivist"); got != 95.0 {
		t.Errorf("expected Archivist floor 95.0, got %f", got)
	}
	if got := QualityFloorForPreset("archivist-custom"); got != 95.0 {
		t.Errorf("expected archivist floor 95.0, got %f", got)
	}
	if got := QualityFloorForPreset("Balanced"); got != 92.0 {
		t.Errorf("expected Balanced floor 92.0, got %f", got)
	}
	if got := QualityFloorForPreset("Maximum Space Savings"); got != 92.0 {
		t.Errorf("expected default floor 92.0, got %f", got)
	}
}

// TestRecordCommand exercises the trace collector: phase labeling, the
// same-phase Count collapse used by per-chunk encodes, and phase switches.
func TestRecordCommand(t *testing.T) {
	var trace []CommandRecord
	job := TranscodeJob{Trace: &trace, TracePhase: "encode:chunk"}

	job.recordCommand([]string{"-i", "a.mkv", "a-out.mkv"})
	job.recordCommand([]string{"-i", "b.mkv", "b-out.mkv"})
	job.recordCommand([]string{"-i", "c.mkv", "c-out.mkv"})

	if len(trace) != 1 {
		t.Fatalf("expected 1 collapsed record, got %d", len(trace))
	}
	if trace[0].Phase != "encode:chunk" {
		t.Errorf("phase = %q, want encode:chunk", trace[0].Phase)
	}
	if trace[0].Count != 3 {
		t.Errorf("count = %d, want 3", trace[0].Count)
	}
	if got := trace[0].Argv[1]; got != "a.mkv" {
		t.Errorf("first record argv should keep the first chunk's paths, got %q", got)
	}

	// A different phase starts a new record.
	job.TracePhase = "concat"
	job.recordCommand([]string{"-f", "concat"})
	if len(trace) != 2 {
		t.Fatalf("expected 2 records after phase switch, got %d", len(trace))
	}
	if trace[1].Count != 1 {
		t.Errorf("single-invocation phase should have count 1, got %d", trace[1].Count)
	}

	// A nil trace must not panic.
	plain := TranscodeJob{}
	plain.recordCommand([]string{"-version"})
}

// TestRecordCommandOnTrace verifies the phase-start callback fires once per
// NEW phase record (not on Count increments), so mid-flight persistence
// stays bounded.
func TestRecordCommandOnTrace(t *testing.T) {
	var trace []CommandRecord
	var fired [][]CommandRecord
	job := TranscodeJob{Trace: &trace, TracePhase: "encode:chunk"}
	job.OnTrace = func(tr []CommandRecord) {
		fired = append(fired, append([]CommandRecord{}, tr...))
	}

	job.recordCommand([]string{"-i", "a.mkv"})
	job.recordCommand([]string{"-i", "b.mkv"}) // Count increment: no callback
	job.TracePhase = "concat"
	job.recordCommand([]string{"-f", "concat"})

	if len(fired) != 2 {
		t.Fatalf("OnTrace fired %d times, want 2 (new record per phase)", len(fired))
	}
	if len(fired[0]) != 1 || fired[0][0].Phase != "encode:chunk" {
		t.Errorf("first callback = %+v, want single encode:chunk record", fired[0])
	}
	if len(fired[1]) != 2 || fired[1][1].Phase != "concat" {
		t.Errorf("second callback = %+v, want trace grown to 2 records", fired[1])
	}
	// The callback must receive a copy: later mutations of the trace must
	// not rewrite what was already delivered.
	if fired[0][0].Count != 1 {
		t.Errorf("delivered record mutated after the fact: count = %d, want 1", fired[0][0].Count)
	}
}

// TestRecordCommandDefaultPhase verifies an unlabeled job records under the
// "encode" phase.
func TestRecordCommandDefaultPhase(t *testing.T) {
	var trace []CommandRecord
	job := TranscodeJob{Trace: &trace}
	job.recordCommand([]string{"-i", "in.mkv"})
	if len(trace) != 1 || trace[0].Phase != "encode" {
		t.Fatalf("default phase record = %+v, want single encode record", trace)
	}
}
