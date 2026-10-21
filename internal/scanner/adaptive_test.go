package scanner

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
)

func TestAdaptiveControllerScaling(t *testing.T) {
	cfg := AdaptiveConfig{
		MinWorkers:       1,
		MaxWorkers:       3,
		InitialWorkers:   1,
		TargetLatency:    1000 * time.Millisecond,
		FastLatency:      300 * time.Millisecond,
		ScaleUpStreak:    2,
		CPULoadThreshold: 100.0,
	}

	ctrl := NewAdaptiveController(cfg)
	if ctrl.CurrentWorkers() != 1 {
		t.Fatalf("Expected initial workers 1, got %d", ctrl.CurrentWorkers())
	}

	// 1st fast observation: should not scale up yet (needs streak of 2)
	w := ctrl.ReportObservation(100*time.Millisecond, false)
	if w != 1 {
		t.Errorf("Expected 1 worker after 1 fast probe, got %d", w)
	}

	// 2nd fast observation: should scale up to 2
	w = ctrl.ReportObservation(100*time.Millisecond, false)
	if w != 2 {
		t.Errorf("Expected 2 workers after streak reached, got %d", w)
	}

	// High latency spike: should scale down immediately
	w = ctrl.ReportObservation(1500*time.Millisecond, false)
	if w != 1 {
		t.Errorf("Expected 1 worker after latency spike, got %d", w)
	}

	// Scale back up to 2: provide fast probes until EWMA settles and streak is satisfied
	for i := 0; i < 5; i++ {
		ctrl.ReportObservation(100*time.Millisecond, false)
	}
	if ctrl.CurrentWorkers() != 2 {
		t.Fatalf("Expected 2 workers before error, got %d", ctrl.CurrentWorkers())
	}

	// Error occurs
	w = ctrl.ReportObservation(50*time.Millisecond, true)
	if w != 1 {
		t.Errorf("Expected 1 worker after error, got %d", w)
	}
}

func TestAdaptiveScanFiles(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "adaptivetest_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	var paths []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(tempDir, "sample"+string(rune('1'+i))+".mp4")
		if err := os.WriteFile(p, []byte("test content"), 0644); err != nil {
			t.Fatalf("Failed to write file: %v", err)
		}
		paths = append(paths, p)
	}

	var observedCallbacks atomic.Int64
	results, err := AdaptiveScanFiles(paths, "", func(file string, info ffmpeg.MediaInfo, activeWorkers int, maxWorkers int, completed int, total int) {
		observedCallbacks.Add(1)
		if activeWorkers < 1 {
			t.Errorf("Expected at least 1 active worker, got %d", activeWorkers)
		}
		if maxWorkers < 1 {
			t.Errorf("Expected at least 1 max worker, got %d", maxWorkers)
		}
	})

	if err != nil {
		t.Fatalf("AdaptiveScanFiles failed: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("Expected 5 results, got %d", len(results))
	}
	if observedCallbacks.Load() != 5 {
		t.Fatalf("Expected 5 callbacks, got %d", observedCallbacks.Load())
	}
}

func TestAdaptiveScanFilesRecordsExplicitErrorOnProbeFailures(t *testing.T) {
	tempDir := t.TempDir()
	p := filepath.Join(tempDir, "sample.mp4")
	if err := os.WriteFile(p, []byte("invalid content"), 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	// Passing a non-existent ffprobe path will cause ProbeMedia to return an error
	results, err := AdaptiveScanFiles([]string{p}, "/does/not/exist/ffprobe", nil)
	if err != nil {
		t.Fatalf("AdaptiveScanFiles failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Expected 1 result for failed probe, got %d", len(results))
	}
	if results[0].Codec != "error" {
		t.Errorf("Expected Codec to be %q, got %q", "error", results[0].Codec)
	}
	if results[0].Error == "" {
		t.Errorf("Expected non-empty Error for probe failure, got empty")
	}
	if results[0].FilePath != p {
		t.Errorf("Expected FilePath %q, got %q", p, results[0].FilePath)
	}
}

func TestAdaptiveScanFilesHoldsConcurrencySlotAcrossHashing(t *testing.T) {
	tempDir := t.TempDir()
	var paths []string
	for i := 0; i < 8; i++ {
		p := filepath.Join(tempDir, "video_"+string(rune('a'+i))+".mkv")
		// Write 1.5MB to exercise multi-chunk fingerprinting
		data := make([]byte, 1500*1024)
		for j := range data {
			data[j] = byte((i + j) % 256)
		}
		if err := os.WriteFile(p, data, 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
		paths = append(paths, p)
	}

	results, err := AdaptiveScanFiles(paths, "", nil)
	if err != nil {
		t.Fatalf("AdaptiveScanFiles failed: %v", err)
	}
	if len(results) != len(paths) {
		t.Fatalf("Expected %d results, got %d", len(paths), len(results))
	}
	for _, r := range results {
		if r.Hash == "" {
			t.Errorf("Expected fingerprint hash for %s, got empty", r.FilePath)
		}
		if r.SizeBytes != 1500*1024 {
			t.Errorf("Expected size 1500KB, got %d", r.SizeBytes)
		}
	}
}

