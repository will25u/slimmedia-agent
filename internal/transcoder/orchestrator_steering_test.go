package transcoder

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
)

// TestOrchestrator_SteeringFromTopology (issue #201): the orchestrator
// derives each software encode job's ThreadSteering from the machine
// topology, keyed off the RESOLVED encoder (the daemon dispatches codec
// family strings like "hevc"; steering must engage for the resolved
// "libx265"). Symmetric topologies and hardware encoders get nil (untouched).
func TestOrchestrator_SteeringFromTopology(t *testing.T) {
	repoFFprobe := repoBin(t, "ffprobe")

	src, err := os.ReadFile(filepath.Join(filepath.Dir(repoFFprobe), "..", "test.mkv"))
	if err != nil {
		t.Skipf("repo test.mkv not available: %v", err)
	}

	hybrid := hardware.CPUTopology{
		Vendor: "GenuineIntel", Hybrid: true,
		PerformanceCPUs: []int{8, 9, 10, 11, 12, 13, 14, 15},
	}

	tests := []struct {
		name      string
		topo      hardware.CPUTopology
		family    string // daemon-style codec family, as dispatched by the worker
		engine    string // spec.EncoderEngine ("cpu", "nvenc", ...)
		caps      ffmpeg.EncoderCaps
		wantSteer bool
	}{
		{
			name:      "hybrid intel cpu engine resolves to libx265 and steers",
			topo:      hybrid,
			family:    "hevc",
			engine:    "cpu",
			caps:      ffmpeg.EncoderCaps{HasLibx265: true},
			wantSteer: true,
		},
		{
			name:      "hybrid intel cpu engine resolves to libx264 and steers",
			topo:      hybrid,
			family:    "h264",
			engine:    "cpu",
			caps:      ffmpeg.EncoderCaps{HasLibx264: true},
			wantSteer: true,
		},
		{
			name:      "hybrid intel nvenc engine stays untouched",
			topo:      hybrid,
			family:    "hevc",
			engine:    "nvenc",
			caps:      ffmpeg.EncoderCaps{HasNvencHEVC: true, HasNvidia: true},
			wantSteer: false,
		},
		{
			name:      "symmetric topology no steering",
			topo:      hardware.CPUTopology{Vendor: "AuthenticAMD", Symmetric: true},
			family:    "hevc",
			engine:    "cpu",
			caps:      ffmpeg.EncoderCaps{HasLibx265: true},
			wantSteer: false,
		},
		{
			name:      "zero topology no steering",
			topo:      hardware.CPUTopology{},
			family:    "hevc",
			engine:    "cpu",
			caps:      ffmpeg.EncoderCaps{HasLibx265: true},
			wantSteer: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			inputPath := filepath.Join(tempDir, "input.mkv")
			if err := os.WriteFile(inputPath, src, 0644); err != nil {
				t.Fatalf("stage input: %v", err)
			}
			canonical, err := CanonicalMediaPath(inputPath)
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}

			o := NewOrchestrator(
				NewStateMachine(nil),
				NewPipeline(filepath.Join(tempDir, "scratch"), filepath.Join(tempDir, "quarantine"), 0),
				filepath.Join(filepath.Dir(repoFFprobe), "ffmpeg"),
			)
			o.Library = fakeLibrary{canonical: true}
			o.Caps = tt.caps
			o.CPUTopo = tt.topo

			steerCh := make(chan *ffmpeg.ThreadSteering, 1)
			o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
				steerCh <- job.Steering
				if job.ProgressChan != nil {
					close(job.ProgressChan) // RunTranscode's closure guarantee
				}
				return nil
			}

			job := &Job{ID: "job-steer", FilePath: inputPath}
			_, _ = o.ProcessJobWithSpec(context.Background(), job, tt.family,
				jobspec.JobSpec{CRF: 30, PresetSpeed: "ultrafast", EncoderEngine: tt.engine}, JobHooks{}, nil)

			var got *ffmpeg.ThreadSteering
			select {
			case got = <-steerCh:
			default:
				t.Fatal("runner was not invoked")
			}
			if !tt.wantSteer {
				if got != nil {
					t.Fatalf("steering = %+v, want nil", got)
				}
				return
			}
			if got == nil || !got.Enabled {
				t.Fatalf("steering = %+v, want enabled", got)
			}
			wantCPUs := tt.topo.PerformanceCPUs
			if len(got.CPUs) != len(wantCPUs) {
				t.Fatalf("steering CPUs = %v, want %v", got.CPUs, wantCPUs)
			}
		})
	}
}
