package ffmpeg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// singlePassFakeFFmpeg installs a fake ffmpeg that records every invocation's
// argv and a start timestamp, then sleeps — long enough for a pause request
// to land mid-encode — and exits 0. It has no segment/demux materialization:
// any invocation reaching it is an encode pass.
func singlePassFakeFFmpeg(t *testing.T, dir string) (string, string) {
	t.Helper()
	argsFile := filepath.Join(dir, "args.bin")
	startsFile := filepath.Join(dir, "starts.log")
	script := fmt.Sprintf(`#!/bin/sh
{ for a in "$@"; do printf '%%s\000' "$a"; done; printf '\000'; } >> %q
date +%%s%%N >> %q
sleep 0.5
exit 0
`, argsFile, startsFile)
	return fakeTool(t, dir, "ffmpeg", script), argsFile
}

func nvencTestCaps() *EncoderCaps {
	return &EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: true, HasLibx265: true, RecommendedEncoder: "hevc_nvenc"}
}

// TestRunTranscodeSinglePassPauseCancelsAndRestarts is the single-pass pause
// contract: a PAUSE_JOB arriving mid-encode cancels the running pass,
// reports PAUSED, holds further work, and requeues the encode — restarts it
// from the beginning — after RESUME_JOB.
func TestRunTranscodeSinglePassPauseCancelsAndRestarts(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg, _ := singlePassFakeFFmpeg(t, tmp)
	fakeToolChunkAwareFFprobe(t, tmp)

	var paused atomic.Bool
	var mu sync.Mutex
	var pauseEvents []bool

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "nvenc",
			QualityTarget: 22,
		},
		Caps:         nvencTestCaps(),
		DurationSec:  600,
		PollInterval: 10 * time.Millisecond,
		IsPaused:     func() bool { return paused.Load() },
		OnPause: func(p bool) {
			mu.Lock()
			pauseEvents = append(pauseEvents, p)
			mu.Unlock()
		},
	}

	start := time.Now()
	go func() {
		time.Sleep(200 * time.Millisecond)
		paused.Store(true)
	}()
	go func() {
		time.Sleep(1300 * time.Millisecond)
		paused.Store(false)
	}()

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	mu.Lock()
	if len(pauseEvents) != 2 || pauseEvents[0] != true || pauseEvents[1] != false {
		t.Fatalf("expected pause events [true false], got %v", pauseEvents)
	}
	mu.Unlock()

	// The first pass must have been cancelled and the encode restarted:
	// exactly two encode invocations, and the second starts only after the
	// ~1.3s paused window.
	data, err := os.ReadFile(filepath.Join(tmp, "starts.log"))
	if err != nil {
		t.Fatalf("fake ffmpeg recorded no invocations: %v", err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(data)), "\n")); n != 2 {
		t.Fatalf("expected 2 encode invocations (cancel + restart), got %d", n)
	}
	if elapsed := time.Since(start); elapsed < 1300*time.Millisecond {
		t.Fatalf("encode restarted after only %v — pause hold was not honored", elapsed)
	}
}

// TestRunTranscodeSinglePassQuietHoursHoldsEncode: a quiet-hours schedule
// going active mid-encode cancels the pass exactly like a pause flag; the
// encode requeues once the schedule clears.
func TestRunTranscodeSinglePassQuietHoursHoldsEncode(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg, _ := singlePassFakeFFmpeg(t, tmp)
	fakeToolChunkAwareFFprobe(t, tmp)

	quietSched := &protocol.ScheduleConfig{
		QuietHoursEnabled: true,
		ScheduleGrid:      protocol.WeeklySchedule(strings.Repeat("1", 168)),
	}
	var sched atomic.Pointer[protocol.ScheduleConfig]
	var mu sync.Mutex
	var pauseEvents []bool

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "nvenc",
			QualityTarget: 22,
		},
		Caps:         nvencTestCaps(),
		DurationSec:  600,
		PollInterval: 10 * time.Millisecond,
		GetSchedule:  func() *protocol.ScheduleConfig { return sched.Load() },
		OnPause: func(p bool) {
			mu.Lock()
			pauseEvents = append(pauseEvents, p)
			mu.Unlock()
		},
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		sched.Store(quietSched)
	}()
	go func() {
		time.Sleep(1300 * time.Millisecond)
		sched.Store(nil)
	}()

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	mu.Lock()
	if len(pauseEvents) != 2 || pauseEvents[0] != true || pauseEvents[1] != false {
		t.Fatalf("expected pause events [true false], got %v", pauseEvents)
	}
	mu.Unlock()

	data, err := os.ReadFile(filepath.Join(tmp, "starts.log"))
	if err != nil {
		t.Fatalf("fake ffmpeg recorded no invocations: %v", err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(data)), "\n")); n != 2 {
		t.Fatalf("expected 2 encode invocations (cancel + restart), got %d", n)
	}
}

// TestRunTranscodeOverrideForcesSinglePassOnSoftware: the spec-level override
// runs a software-resolved job as one full-stream pass. A chunk-phase-locked
// custom flag (-fps_mode) that the chunked pipeline rejects is honored here.
func TestRunTranscodeOverrideForcesSinglePassOnSoftware(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg, argsFile := singlePassFakeFFmpeg(t, tmp)
	fakeToolChunkAwareFFprobe(t, tmp)

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
			Chunking:      jobspec.ChunkingSinglePass,
			CustomFlags:   "-fps_mode cfr",
		},
		Caps:         &EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"},
		DurationSec:  600,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	invocations := readFakeArgs(t, argsFile)
	// One custom-flags dry-run validation plus exactly one encode pass.
	if len(invocations) != 2 {
		t.Fatalf("expected 2 invocations (dry-run + single encode), got %d", len(invocations))
	}
	encode := invocations[1]
	assertHasArg(t, encode, "-fps_mode")
	assertHasArg(t, encode, "cfr")
	for _, tok := range encode {
		if tok == "segment" {
			t.Fatalf("single-pass encode must not use the segment muxer: %q", encode)
		}
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	if _, err := os.Stat(paths.SegmentDir()); !os.IsNotExist(err) {
		t.Errorf("single-pass job must not create the segment directory, but %s exists", paths.SegmentDir())
	}
}

// TestRunTranscodeOverrideForcesChunkedOnHardware: the opposite override —
// a hardware-resolved job forced through the chunked pipeline (demux, split,
// chunk encode, concat) so future presets and tests can pin either shape.
func TestRunTranscodeOverrideForcesChunkedOnHardware(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 1)
	fakeToolChunkAwareFFprobe(t, tmp)

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "nvenc",
			QualityTarget: 22,
			Chunking:      jobspec.ChunkingChunked,
		},
		Caps:         nvencTestCaps(),
		DurationSec:  600,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	invocations := readFakeArgs(t, argsFile)
	if len(invocations) != 4 {
		t.Fatalf("expected 4 invocations (demux + split + chunk encode + concat), got %d", len(invocations))
	}
	sawSegment := false
	for _, inv := range invocations {
		for i, tok := range inv {
			if tok == "-f" && i+1 < len(inv) && inv[i+1] == "segment" {
				sawSegment = true
			}
		}
	}
	if !sawSegment {
		t.Errorf("chunked override must run the segment-muxer split pass; invocations: %q", invocations)
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	if _, err := os.Stat(paths.SegmentDir()); err != nil {
		t.Errorf("chunked override must create the segment directory: %v", err)
	}
}
