package ffmpeg

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// archivistWirePayload returns the spec_version:2 wire payload the API's
// resolution engine emits for a job created with the seeded "The Archivist
// (Hardware)" builtin preset (migration 000036), tagged with the code-owned
// Archivist quality tier. The payload travels the same decode path a daemon
// START_JOB frame takes: JSON unmarshal into protocol.StartJobPayload, then
// jobspec.FromWire.
func archivistWirePayload(t *testing.T, engine string) jobspec.JobSpec {
	t.Helper()
	raw := fmt.Sprintf(`{
		"job_id": "archivist-e2e",
		"input_path": "/media/in.mkv",
		"spec_version": 2,
		"video": {"target_codec": "hevc", "encoder_engine": %q, "crf": 18, "preset_speed": "slow", "color_depth": "10bit", "quality_tier": "archivist"},
		"audio": {"codec_policy": "copy_all"},
		"subtitles": {"format_policy": "copy_all"},
		"container": {"format": "mkv", "output_file_strategy": "alongside", "action_on_success": "keep"}
	}`, engine)
	var p protocol.StartJobPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("archivist wire payload failed to unmarshal: %v", err)
	}
	spec, err := jobspec.FromWire(p)
	if err != nil {
		t.Fatalf("jobspec.FromWire: %v", err)
	}
	return spec
}

// TestRunTranscodeArchivistEngineScenarios is the end-to-end proof that an
// Archivist job produces the correct vendor command line on every agent
// hardware scenario. Each scenario runs the real RunTranscode entry point
// against a recording fake ffmpeg, starting from a wire-shaped payload, so
// the asserted argv covers the whole chain: wire decode -> jobspec -> encoder
// resolution -> Archivist package -> exec. The fallback scenarios prove the
// stepdown stays at Archivist grade instead of degrading to Balanced-tier
// defaults.
func TestRunTranscodeArchivistEngineScenarios(t *testing.T) {
	nvencPackage := []string{
		"-c:v hevc_nvenc",
		"-rc:v vbr -cq:v 20 -b:v 0",
		"-tune hq -multipass fullres -spatial-aq 1 -temporal-aq 1 -b_ref_mode middle",
		"-preset p7",
	}
	qsvPackage := []string{
		"-c:v hevc_qsv",
		"-rc icq -global_quality 13",
		"-extbrc 1 -adaptive_i 1 -adaptive_b 1",
		"-preset slower",
	}
	cpuPackage := []string{
		"-c:v libx265",
		"-crf 18",
		"-preset slow",
		"-x265-params aq-mode=3:sao=0:tune=film",
	}
	hwMarks := []string{"hevc_nvenc", "hevc_qsv", "-cq:v", "-global_quality", "-extbrc"}

	scenarios := []struct {
		name string
		// engine is the encoder_engine the payload carries. "auto" is what
		// the seeded preset ships, resolved per-agent; a concrete vendor is
		// what the API pins when it dispatches to a known-hardware agent.
		engine string
		caps   EncoderCaps
		// Software-resolving scenarios force the single-pass pipeline shape
		// (the spec-level test override) so the fake ffmpeg only ever sees
		// the encode argv; the chunked pipeline shape has its own suites.
		singlePass bool
		want       []string
		without    []string
	}{
		{
			name:   "nvidia agent runs the nvenc archivist package",
			engine: "auto",
			caps: EncoderCaps{
				HasNvencHEVC: true, HasNvencTuningVerified: true,
				RecommendedEncoder: "hevc_nvenc",
			},
			want:    nvencPackage,
			without: []string{"hevc_qsv", "libx265", "-global_quality", "-extbrc", "-adaptive_i"},
		},
		{
			name:   "intel agent runs the qsv archivist package",
			engine: "auto",
			caps: EncoderCaps{
				HasIntel: true, HasQSVHEVC: true,
				HasQSVExtBRCVerified: true, HasQSVAdaptiveIBVerified: true,
				RecommendedEncoder: "hevc_qsv",
			},
			want:    qsvPackage,
			without: []string{"hevc_nvenc", "-cq:v", "look_ahead", "libx265"},
		},
		{
			name:   "cpu-only agent runs the software archivist package",
			engine: "auto",
			caps: EncoderCaps{
				HasLibx265: true, RecommendedEncoder: "libx265",
			},
			singlePass: true,
			want:       cpuPackage,
			without:    hwMarks,
		},
		{
			name:   "nvenc requested on an intel-only agent falls back to the qsv archivist package",
			engine: "nvenc",
			caps: EncoderCaps{
				HasIntel: true, HasQSVHEVC: true,
				HasQSVExtBRCVerified: true, HasQSVAdaptiveIBVerified: true,
				RecommendedEncoder: "hevc_qsv",
			},
			want:    qsvPackage,
			without: []string{"hevc_nvenc", "-cq:v"},
		},
		{
			name:   "nvenc requested with no hardware falls back to the software archivist package",
			engine: "nvenc",
			caps: EncoderCaps{
				HasLibx265: true, RecommendedEncoder: "libx265",
			},
			singlePass: true,
			want:       cpuPackage,
			without:    hwMarks,
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			tmp := t.TempDir()
			fakeFFmpeg, argsFile := singlePassFakeFFmpeg(t, tmp)
			fakeToolChunkAwareFFprobe(t, tmp)

			spec := archivistWirePayload(t, sc.engine)
			if sc.singlePass {
				spec.Chunking = jobspec.ChunkingSinglePass
			}
			caps := sc.caps
			job := TranscodeJob{
				FFmpegPath:   fakeFFmpeg,
				InputFile:    filepath.Join(tmp, "in.mkv"),
				OutputFile:   filepath.Join(tmp, "out.mkv"),
				Codec:        "hevc",
				Spec:         spec,
				Caps:         &caps,
				DurationSec:  600,
				PollInterval: 10 * time.Millisecond,
			}

			if err := runWithTimeout(t, job); err != nil {
				t.Fatalf("RunTranscode: %v", err)
			}

			invocations := readFakeArgs(t, argsFile)
			if len(invocations) != 1 {
				t.Fatalf("expected exactly 1 encode invocation, got %d: %q", len(invocations), invocations)
			}
			joined := strings.Join(invocations[0], " ")
			for _, piece := range sc.want {
				if !strings.Contains(joined, piece) {
					t.Errorf("archivist argv missing %q:\n%s", piece, joined)
				}
			}
			for _, absent := range sc.without {
				if strings.Contains(joined, absent) {
					t.Errorf("archivist argv must not contain %q:\n%s", absent, joined)
				}
			}
		})
	}
}
