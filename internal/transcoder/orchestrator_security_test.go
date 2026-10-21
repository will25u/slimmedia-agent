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

// fakeLibrary is an in-memory ScannedLibrary for tests.
type fakeLibrary map[string]bool

func (f fakeLibrary) Contains(canonicalPath string) (bool, error) {
	return f[canonicalPath], nil
}

// repoBin returns the path of a repo-root bin tool from a package test, or
// skips the test when the binaries are unavailable.
func repoBin(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "bin", name))
	if err != nil {
		t.Fatalf("failed to resolve bin path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s not available: %v", path, err)
	}
	return path
}

func newTestOrchestrator(t *testing.T, lib ScannedLibrary) (*Orchestrator, string, string) {
	t.Helper()
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	ffmpegPath := repoBin(t, "ffmpeg")

	o := NewOrchestrator(NewStateMachine(nil), NewPipeline(scratchDir, quarantineDir, 0), ffmpegPath)
	o.Library = lib
	return o, scratchDir, quarantineDir
}

func scratchEntries(t *testing.T, scratchDir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(scratchDir)
	if err != nil {
		t.Fatalf("failed to read scratch dir: %v", err)
	}
	return entries
}

func quarantineEntries(t *testing.T, quarantineDir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(quarantineDir)
	if err != nil {
		t.Fatalf("failed to read quarantine dir: %v", err)
	}
	return entries
}

// Positive path: the full transcode -> verify -> quarantine -> replace
// cycle against the real ffmpeg binary using the repo's test.mkv.
func TestProcessJob_EndToEnd(t *testing.T) {
	ffmpegPath := repoBin(t, "ffmpeg")

	mediaDir := t.TempDir()
	src, err := os.ReadFile(filepath.Join(filepath.Dir(ffmpegPath), "..", "test.mkv"))
	if err != nil {
		t.Fatalf("failed to read test.mkv: %v", err)
	}
	inputPath := filepath.Join(mediaDir, "test movie.mkv")
	if err := os.WriteFile(inputPath, src, 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	o, scratchDir, quarantineDir := newTestOrchestrator(t, fakeLibrary{})
	canonical, err := CanonicalMediaPath(inputPath)
	if err != nil {
		t.Fatalf("canonicalization failed: %v", err)
	}
	o.Library = fakeLibrary{canonical: true}

	job := &Job{ID: "job-e2e", FilePath: inputPath}
	// High CRF keeps the tiny 1s test clip's output well inside the
	// pipeline's 5%-110% size sanity band.
	result, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", jobspec.JobSpec{CRF: 45, PresetSpeed: "ultrafast"}, JobHooks{}, nil)
	if err != nil {
		t.Fatalf("ProcessJob failed: %v", err)
	}
	if job.State != models.StateCompleted {
		t.Fatalf("expected completed state, got %s (%s)", job.State, job.ErrorMessage)
	}

	// The library file now holds the transcoded output...
	replaced, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("failed to read replaced file: %v", err)
	}
	if len(replaced) == 0 || string(replaced) == string(src) {
		t.Fatalf("expected replaced file to differ from source")
	}
	if result.NewSize != int64(len(replaced)) {
		t.Fatalf("result NewSize %d does not match placed file %d", result.NewSize, len(replaced))
	}

	// ...the original is recoverable byte-for-byte from the namespaced
	// quarantine slot...
	q := quarantineEntries(t, quarantineDir)
	if len(q) != 1 {
		t.Fatalf("expected 1 quarantine entry, got %d", len(q))
	}
	wantName := filepath.Base(jobpaths.New(scratchDir, quarantineDir, canonical).Quarantine())
	if q[0].Name() != wantName {
		t.Fatalf("expected quarantine entry %q, got %q", wantName, q[0].Name())
	}
	quarantined, err := os.ReadFile(filepath.Join(quarantineDir, wantName))
	if err != nil {
		t.Fatalf("failed to read quarantine copy: %v", err)
	}
	if string(quarantined) != string(src) {
		t.Fatalf("quarantined original does not match source bytes")
	}

	// ...and the scratch dir is clean.
	if n := scratchEntries(t, scratchDir); len(n) != 0 {
		t.Fatalf("expected empty scratch dir, got %d entries: %v", len(n), n)
	}

	// The placed output must be valid media (it replaced the original).
	if _, err := ffmpeg.ProbeMedia(filepath.Join(filepath.Dir(ffmpegPath), "ffprobe"), canonical); err != nil {
		t.Fatalf("placed output is not decodable: %v", err)
	}
}

// (a) A job whose input path is outside the scanned library set must be
// rejected before any filesystem mutation.
func TestProcessJob_RejectsPathOutsideLibrary(t *testing.T) {
	mediaDir := t.TempDir()
	inputPath := filepath.Join(mediaDir, "unscanned.mkv")
	original := []byte("PRECIOUS_ORIGINAL_CONTENT")
	if err := os.WriteFile(inputPath, original, 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	o, scratchDir, quarantineDir := newTestOrchestrator(t, fakeLibrary{}) // empty allowlist

	job := &Job{ID: "job-outside", FilePath: inputPath}
	err := o.ProcessJob(context.Background(), job, "hevc")
	if err == nil {
		t.Fatalf("expected rejection for unscanned path")
	}
	if !strings.Contains(err.Error(), "outside the scanned media library") {
		t.Fatalf("unexpected error: %v", err)
	}

	// No mutation anywhere: file untouched, no scratch or quarantine writes.
	got, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("input file unreadable: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("input file was mutated")
	}
	if n := scratchEntries(t, scratchDir); len(n) != 0 {
		t.Fatalf("scratch dir was written: %d entries", len(n))
	}
	if n := quarantineEntries(t, quarantineDir); len(n) != 0 {
		t.Fatalf("quarantine dir was written: %d entries", len(n))
	}
}

// (d) A corrupted transcode output that happens to match the original's
// size must fail verification; the original must not be deleted.
func TestProcessJob_CorruptSameSizeOutputRejected(t *testing.T) {
	mediaDir := t.TempDir()
	inputPath := filepath.Join(mediaDir, "movie.mkv")
	original := []byte("FAKE_VIDEO_CONTENT_FOR_TRANSCODE_TESTING")
	if err := os.WriteFile(inputPath, original, 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	o, scratchDir, quarantineDir := newTestOrchestrator(t, fakeLibrary{})
	canonical, err := CanonicalMediaPath(inputPath)
	if err != nil {
		t.Fatalf("canonicalization failed: %v", err)
	}
	o.Library = fakeLibrary{canonical: true}

	// Runner that reports success but writes same-size garbage.
	o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
		garbage := make([]byte, len(original))
		for i := range garbage {
			garbage[i] = 0xAA
		}
		if err := os.WriteFile(job.OutputFile, garbage, 0644); err != nil {
			return err
		}
		close(job.ProgressChan)
		return nil
	}

	job := &Job{ID: "job-corrupt", FilePath: inputPath}
	err = o.ProcessJob(context.Background(), job, "hevc")
	if err == nil {
		t.Fatalf("expected verification failure for corrupt output")
	}
	if job.State != models.StatePermFailed && job.State != models.StateFailed {
		t.Fatalf("expected failed state, got %s", job.State)
	}

	// Original untouched, quarantine empty, scratch cleaned up.
	got, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("original unreadable: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("original was modified")
	}
	if n := quarantineEntries(t, quarantineDir); len(n) != 0 {
		t.Fatalf("quarantine dir was written: %d entries", len(n))
	}
	scratchPath := jobpaths.New(scratchDir, quarantineDir, canonical).Scratch()
	if _, err := os.Stat(scratchPath); !os.IsNotExist(err) {
		t.Fatalf("scratch file not cleaned up")
	}
	// Per issue #69: on verification failure, .segments dir is purged
	entries := scratchEntries(t, scratchDir)
	if len(entries) != 0 {
		t.Fatalf("expected empty scratch dir after verification failure, got: %v", entries)
	}
}

// A protocol/URL input must be rejected at confinement time, before any
// filesystem work happens.
func TestProcessJob_RejectsProtocolInputBeforeAnyWork(t *testing.T) {
	o, scratchDir, quarantineDir := newTestOrchestrator(t, nil)

	job := &Job{ID: "job-ssrf", FilePath: "http://169.254.169.254/x"}
	err := o.ProcessJob(context.Background(), job, "hevc")
	if err == nil {
		t.Fatalf("expected SSRF path to be rejected")
	}
	if n := scratchEntries(t, scratchDir); len(n) != 0 {
		t.Fatalf("scratch dir was written: %d entries", len(n))
	}
	if n := quarantineEntries(t, quarantineDir); len(n) != 0 {
		t.Fatalf("quarantine dir was written: %d entries", len(n))
	}
}

func TestProcessJob_RejectsInvalidCustomFlagsBeforeAnyWork(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.mkv")
	if err := os.WriteFile(inputPath, []byte("fake video content"), 0644); err != nil {
		t.Fatalf("failed to create input file: %v", err)
	}

	o, scratchDir, quarantineDir := newTestOrchestrator(t, fakeLibrary{inputPath: true})

	spec := jobspec.JobSpec{
		CustomFlags: "-preset ultrafast /tmp/extra.mkv",
	}
	job := &Job{ID: "job-bad-flags", FilePath: inputPath}
	_, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", spec, JobHooks{}, nil)
	if err == nil {
		t.Fatalf("expected invalid custom flags to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid custom flags") {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.State != models.StateSkipped {
		t.Fatalf("expected state Skipped, got %s", job.State)
	}
	if n := scratchEntries(t, scratchDir); len(n) != 0 {
		t.Fatalf("scratch dir was written: %d entries", len(n))
	}
	if n := quarantineEntries(t, quarantineDir); len(n) != 0 {
		t.Fatalf("quarantine dir was written: %d entries", len(n))
	}
}

func TestProcessJob_RejectsUnrecognizedOutputFileStrategy(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.mkv")
	originalContent := []byte("original media content")
	if err := os.WriteFile(inputPath, originalContent, 0644); err != nil {
		t.Fatalf("failed to create input file: %v", err)
	}

	o, scratchDir, quarantineDir := newTestOrchestrator(t, fakeLibrary{inputPath: true})

	spec := jobspec.JobSpec{
		OutputFileStrategy: "hacked",
		ActionOnSuccess:    "delete",
	}
	job := &Job{ID: "job-bad-strategy", FilePath: inputPath}
	_, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", spec, JobHooks{}, nil)
	if err == nil {
		t.Fatalf("expected unrecognized strategy to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid destination path") {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.State != models.StateSkipped {
		t.Fatalf("expected state Skipped, got %s", job.State)
	}

	// Verify original file is untouched and not deleted
	data, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("original file was corrupted or deleted: %v", err)
	}
	if string(data) != string(originalContent) {
		t.Fatalf("original file content altered! got %q, want %q", string(data), string(originalContent))
	}

	if n := scratchEntries(t, scratchDir); len(n) != 0 {
		t.Fatalf("scratch dir was written: %d entries", len(n))
	}
	if n := quarantineEntries(t, quarantineDir); len(n) != 0 {
		t.Fatalf("quarantine dir was written: %d entries", len(n))
	}
}

func TestApplyPlacementAndDisposition_RejectsUnrecognizedStrategyAndAction(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.mkv")
	scratchPath := filepath.Join(dir, "scratch.mkv")
	origBytes := []byte("original video content")
	scratchBytes := []byte("scratch transcoded content")
	_ = os.WriteFile(sourcePath, origBytes, 0644)
	_ = os.WriteFile(scratchPath, scratchBytes, 0644)

	pipeline := &Pipeline{
		ScratchDir:    filepath.Join(dir, "scratch"),
		QuarantineDir: filepath.Join(dir, "quarantine"),
	}
	_ = os.MkdirAll(pipeline.ScratchDir, 0755)
	_ = os.MkdirAll(pipeline.QuarantineDir, 0755)

	paths := jobpaths.New(pipeline.ScratchDir, pipeline.QuarantineDir, sourcePath)

	// Unrecognized outputStrategy
	_, err := pipeline.ApplyPlacementAndDisposition(paths, "malicious", "keep", "")
	if err == nil {
		t.Fatalf("expected error for unrecognized output strategy, got nil")
	}

	// Verify original source is untouched
	curSource, err := os.ReadFile(sourcePath)
	if err != nil || string(curSource) != string(origBytes) {
		t.Fatalf("source was modified on unrecognized output strategy error: %v", err)
	}

	// Unrecognized actionOnSuccess
	_, err = pipeline.ApplyPlacementAndDisposition(paths, "alongside", "destroy_everything", "")
	if err == nil {
		t.Fatalf("expected error for unrecognized action on success, got nil")
	}

	// Verify source was not deleted
	curSource, err = os.ReadFile(sourcePath)
	if err != nil || string(curSource) != string(origBytes) {
		t.Fatalf("source was deleted on unrecognized actionOnSuccess error: %v", err)
	}
}
