package transcoder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/models"
)

func TestOrchestrator_DurationVerification(t *testing.T) {
	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)

	// Create a fake ffprobe that returns different duration based on the input file
	// format: "{\"format\":{\"duration\":\"10.00\"}}"
	fakeFFprobe := filepath.Join(binDir, "ffprobe")
	script := `#!/bin/sh
	if echo "$@" | grep -q "scratch"; then
		echo '{"format":{"duration":"5.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
	else
		echo '{"format":{"duration":"10.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
	fi
	`
	_ = os.WriteFile(fakeFFprobe, []byte(script), 0755)

	fakeFFmpeg := filepath.Join(binDir, "ffmpeg")
	_ = os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\nexit 0"), 0755)

	inputPath := filepath.Join(tempDir, "input.mkv")
	_ = os.WriteFile(inputPath, []byte("fake content"), 0644)
	canonical, _ := CanonicalMediaPath(inputPath)

	o := NewOrchestrator(
		NewStateMachine(nil),
		NewPipeline(filepath.Join(tempDir, "scratch"), filepath.Join(tempDir, "quarantine"), 0),
		fakeFFmpeg,
	)
	o.Library = fakeLibrary{canonical: true}
	o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
		defer close(job.ProgressChan)
		return os.WriteFile(job.OutputFile, []byte("123456789012"), 0644)
	}

	job := &Job{ID: "job-dur-fail", FilePath: canonical}
	_, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)

	if err == nil {
		t.Fatalf("expected job to fail due to duration mismatch")
	}
	if !strings.Contains(err.Error(), "duration verification failed") {
		t.Fatalf("expected duration verification error, got: %v", err)
	}
	if job.State != models.StateFailed && job.State != models.StatePermFailed {
		t.Fatalf("expected state %s or %s, got %s", models.StateFailed, models.StatePermFailed, job.State)
	}

	segDir := jobpaths.New(filepath.Join(tempDir, "scratch"), filepath.Join(tempDir, "quarantine"), canonical).SegmentDir()
	if _, err := os.Stat(segDir); !os.IsNotExist(err) {
		t.Fatalf("expected .segments directory to be purged on duration verification failure, but it exists")
	}
}

// TestOrchestrator_DurationVerificationStreamLevel is the regression test for
// healthy chunked transcodes failing verification: the concatenated output's
// container format.duration carries junction muxer/DTS padding (here +3.17s
// over 19 seams of a 20-chunk job) while the video-stream durations match.
// Like-for-like stream comparison must pass the gate; the container diff
// alone must not fail it.
func TestOrchestrator_DurationVerificationStreamLevel(t *testing.T) {
	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)

	fakeFFprobe := filepath.Join(binDir, "ffprobe")
	script := `#!/bin/sh
if echo "$@" | grep -q "scratch"; then
	echo '{"format":{"duration":"5751.960000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"hevc","duration":"5748.790000"}]}'
else
	echo '{"format":{"duration":"5748.790000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264","duration":"5748.790000"}]}'
fi
`
	_ = os.WriteFile(fakeFFprobe, []byte(script), 0755)

	fakeFFmpeg := filepath.Join(binDir, "ffmpeg")
	_ = os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\nexit 0"), 0755)

	inputPath := filepath.Join(tempDir, "input.mkv")
	_ = os.WriteFile(inputPath, []byte("fake content"), 0644)
	canonical, _ := CanonicalMediaPath(inputPath)

	o := NewOrchestrator(
		NewStateMachine(nil),
		NewPipeline(filepath.Join(tempDir, "scratch"), filepath.Join(tempDir, "quarantine"), 0),
		fakeFFmpeg,
	)
	o.Library = fakeLibrary{canonical: true}
	o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
		defer close(job.ProgressChan)
		return os.WriteFile(job.OutputFile, []byte("123456789012"), 0644)
	}

	job := &Job{ID: "job-dur-stream", FilePath: canonical}
	res, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)
	if err != nil {
		t.Fatalf("expected junction padding within stream durations to pass verification, got: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil JobResult")
	}
	if job.State != models.StateCompleted {
		t.Fatalf("expected state %s, got %s", models.StateCompleted, job.State)
	}
}

// TestOrchestrator_DurationVerificationFallbackFlatTolerance covers the
// fallback path (no stream-level durations on either probe): the tolerance is
// a flat ±2s regardless of chunk count (#193), so junction padding on a
// 20-chunk job now fails the gate, a drift within ±2s still passes, and a
// gross duration mismatch fails.
func TestOrchestrator_DurationVerificationFallbackFlatTolerance(t *testing.T) {
	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)

	controlFile := filepath.Join(tempDir, "outdur.txt")
	fakeFFprobe := filepath.Join(binDir, "ffprobe")
	script := `#!/bin/sh
outdur="6003.190000"
if [ -f "` + controlFile + `" ]; then outdur=$(cat "` + controlFile + `"); fi
if echo "$@" | grep -q "scratch"; then
	printf '{"format":{"duration":"%s","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"hevc"}]}\n' "$outdur"
else
	echo '{"format":{"duration":"6000.000000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
fi
`
	_ = os.WriteFile(fakeFFprobe, []byte(script), 0755)

	fakeFFmpeg := filepath.Join(binDir, "ffmpeg")
	_ = os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\nexit 0"), 0755)

	inputPath := filepath.Join(tempDir, "input.mkv")
	_ = os.WriteFile(inputPath, []byte("fake content"), 0644)
	canonical, _ := CanonicalMediaPath(inputPath)

	o := NewOrchestrator(
		NewStateMachine(nil),
		NewPipeline(filepath.Join(tempDir, "scratch"), filepath.Join(tempDir, "quarantine"), 0),
		fakeFFmpeg,
	)
	o.Library = fakeLibrary{canonical: true}
	o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
		defer close(job.ProgressChan)
		return os.WriteFile(job.OutputFile, []byte("123456789012"), 0644)
	}

	// 20 chunks of 300s -> 19 seams; +3.19s of junction padding exceeds the
	// flat ±2s tolerance and must fail — the per-seam inflation that used to
	// absorb it is gone.
	job := &Job{ID: "job-dur-flat-fail", FilePath: canonical}
	_, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)
	if err == nil {
		t.Fatal("expected job to fail duration verification on junction padding beyond the flat ±2s tolerance")
	}
	if !strings.Contains(err.Error(), "duration verification failed") {
		t.Fatalf("expected duration verification error, got: %v", err)
	}

	// A drift within ±2s still passes the mandatory gate.
	_ = os.WriteFile(controlFile, []byte("6001.500000"), 0644)
	jobPass := &Job{ID: "job-dur-flat-pass", FilePath: canonical}
	if _, err := o.ProcessJobWithSpec(context.Background(), jobPass, "hevc", jobspec.JobSpec{}, JobHooks{}, nil); err != nil {
		t.Fatalf("expected drift within ±2s to pass verification, got: %v", err)
	}
	if jobPass.State != models.StateCompleted {
		t.Fatalf("expected state %s, got %s", models.StateCompleted, jobPass.State)
	}

	// A gross mismatch (50s) must still fail the mandatory gate.
	_ = os.WriteFile(controlFile, []byte("6050.000000"), 0644)
	job2 := &Job{ID: "job-dur-flat-gross", FilePath: canonical}
	_, err = o.ProcessJobWithSpec(context.Background(), job2, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)
	if err == nil {
		t.Fatal("expected job to fail duration verification on gross mismatch")
	}
	if !strings.Contains(err.Error(), "duration verification failed") {
		t.Fatalf("expected duration verification error, got: %v", err)
	}
}

// TestOrchestrator_AVAlignmentVerification covers the a/v alignment gate: the
// output's audio-minus-video start-time offset must match the input's within
// ±40ms. A matching offset passes, a shifted one fails the job, and missing
// start-time metadata degrades to a skip rather than a failure.
func TestOrchestrator_AVAlignmentVerification(t *testing.T) {
	run := func(t *testing.T, outAudioStart string) (*Job, error) {
		t.Helper()
		tempDir := t.TempDir()
		binDir := filepath.Join(tempDir, "bin")
		_ = os.MkdirAll(binDir, 0755)

		fakeFFprobe := filepath.Join(binDir, "ffprobe")
		script := `#!/bin/sh
if echo "$@" | grep -q "scratch"; then
	printf '{"format":{"duration":"120.000000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"hevc","duration":"120.000000","start_time":"0.000000"},{"codec_type":"audio","codec_name":"aac","start_time":"` + outAudioStart + `"}]}\n'
else
	echo '{"format":{"duration":"120.000000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264","duration":"120.000000","start_time":"0.000000"},{"codec_type":"audio","codec_name":"aac","start_time":"0.600000"}]}'
fi
`
		_ = os.WriteFile(fakeFFprobe, []byte(script), 0755)

		fakeFFmpeg := filepath.Join(binDir, "ffmpeg")
		_ = os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\nexit 0"), 0755)

		inputPath := filepath.Join(tempDir, "input.mkv")
		_ = os.WriteFile(inputPath, []byte("fake content"), 0644)
		canonical, _ := CanonicalMediaPath(inputPath)

		o := NewOrchestrator(
			NewStateMachine(nil),
			NewPipeline(filepath.Join(tempDir, "scratch"), filepath.Join(tempDir, "quarantine"), 0),
			fakeFFmpeg,
		)
		o.Library = fakeLibrary{canonical: true}
		o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
			defer close(job.ProgressChan)
			return os.WriteFile(job.OutputFile, []byte("123456789012"), 0644)
		}

		job := &Job{ID: "job-av-" + outAudioStart, FilePath: canonical}
		_, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)
		return job, err
	}

	t.Run("matching offset passes", func(t *testing.T) {
		job, err := run(t, "0.600000")
		if err != nil {
			t.Fatalf("expected matching a/v offset to pass verification, got: %v", err)
		}
		if job.State != models.StateCompleted {
			t.Fatalf("expected state %s, got %s", models.StateCompleted, job.State)
		}
	})

	t.Run("shifted offset fails", func(t *testing.T) {
		// Input offset is 0.6s; output shifted to 0.5s -> 100ms desync.
		job, err := run(t, "0.500000")
		if err == nil {
			t.Fatal("expected job to fail a/v alignment verification on shifted offset")
		}
		if !strings.Contains(err.Error(), "a/v alignment verification failed") {
			t.Fatalf("expected a/v alignment error, got: %v", err)
		}
		if job.State != models.StateFailed && job.State != models.StatePermFailed {
			t.Fatalf("expected state %s or %s, got %s", models.StateFailed, models.StatePermFailed, job.State)
		}
	})

	t.Run("missing start times skip the check", func(t *testing.T) {
		tempDir := t.TempDir()
		binDir := filepath.Join(tempDir, "bin")
		_ = os.MkdirAll(binDir, 0755)

		// Identical to the passing fixture but without any start_time fields.
		fakeFFprobe := filepath.Join(binDir, "ffprobe")
		script := `#!/bin/sh
if echo "$@" | grep -q "scratch"; then
	echo '{"format":{"duration":"120.000000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"hevc","duration":"120.000000"},{"codec_type":"audio","codec_name":"aac"}]}'
else
	echo '{"format":{"duration":"120.000000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264","duration":"120.000000"},{"codec_type":"audio","codec_name":"aac"}]}'
fi
`
		_ = os.WriteFile(fakeFFprobe, []byte(script), 0755)

		fakeFFmpeg := filepath.Join(binDir, "ffmpeg")
		_ = os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\nexit 0"), 0755)

		inputPath := filepath.Join(tempDir, "input.mkv")
		_ = os.WriteFile(inputPath, []byte("fake content"), 0644)
		canonical, _ := CanonicalMediaPath(inputPath)

		o := NewOrchestrator(
			NewStateMachine(nil),
			NewPipeline(filepath.Join(tempDir, "scratch"), filepath.Join(tempDir, "quarantine"), 0),
			fakeFFmpeg,
		)
		o.Library = fakeLibrary{canonical: true}
		o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
			defer close(job.ProgressChan)
			return os.WriteFile(job.OutputFile, []byte("123456789012"), 0644)
		}

		job := &Job{ID: "job-av-nostart", FilePath: canonical}
		if _, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", jobspec.JobSpec{}, JobHooks{}, nil); err != nil {
			t.Fatalf("expected missing start times to skip the alignment check, got: %v", err)
		}
		if job.State != models.StateCompleted {
			t.Fatalf("expected state %s, got %s", models.StateCompleted, job.State)
		}
	})
}
