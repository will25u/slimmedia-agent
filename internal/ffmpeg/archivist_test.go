package ffmpeg

import (
	"reflect"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// archivistJobSpec returns the spec the seeded "The Archivist (Hardware)"
// builtin preset resolves to (migration 000036), tagged with the Archivist
// quality tier.
func archivistJobSpec(engine string) jobspec.JobSpec {
	return jobspec.JobSpec{
		Container:        "mkv",
		EncoderEngine:    engine,
		CRF:              18,
		PresetSpeed:      "slow",
		QualityTarget:    18,
		AudioHandling:    "copy_all",
		SubtitleHandling: "copy_all",
		ColorDepth:       "10bit",
		QualityTier:      protocol.QualityTierArchivist,
	}
}

func archivistJob(engine string, caps EncoderCaps) TranscodeJob {
	return TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "hevc",
		Spec:       archivistJobSpec(engine),
		Caps:       &caps,
	}
}

var archivistArgvPrefix = []string{
	"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
	"-protocol_whitelist", "file,pipe", "-i", "/media/in.mkv",
	"-map", "0:v?", "-map", "0:a?", "-map", "0:s?",
	"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
	"-max_muxing_queue_size", "4096", "-copyts", "-c:a", "copy",
	"-c:s", "copy",
}

var archivistArgvSuffix = []string{
	"-progress", "pipe:1", "-nostats", "/media/out.mkv",
}

// TestBuildFfmpegArgsArchivistNVENC covers the Archivist NVIDIA path:
// constant quality 20 via VBR with zero target bitrate, the slowest speed
// preset (p7), and the full quality-tuning package (canary-verified).
func TestBuildFfmpegArgsArchivistNVENC(t *testing.T) {
	caps := EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: true, RecommendedEncoder: "hevc_nvenc"}
	want := append(append(append([]string{},
		archivistArgvPrefix...),
		"-c:v", "hevc_nvenc", "-rc:v", "vbr", "-cq:v", "20", "-b:v", "0",
		"-profile:v", "main10", "-pix_fmt", "p010le",
		"-g", "240",
		"-tune", "hq", "-multipass", "fullres", "-spatial-aq", "1", "-temporal-aq", "1", "-b_ref_mode", "middle",
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709",
		"-preset", "p7"),
		archivistArgvSuffix...)

	got, err := buildFfmpegArgs(archivistJob("nvenc", caps), nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("archivist nvenc argv mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestBuildFfmpegArgsArchivistNVENCStripsUnverifiedTuning proves a driver
// that failed the tuning canary loses the tuning block but keeps the
// Archivist quality lock (CQ 20, p7).
func TestBuildFfmpegArgsArchivistNVENCStripsUnverifiedTuning(t *testing.T) {
	caps := EncoderCaps{HasNvencHEVC: true, RecommendedEncoder: "hevc_nvenc"}
	got, err := buildFfmpegArgs(archivistJob("nvenc", caps), nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	joined := strings.Join(got, " ")
	for _, piece := range []string{"-c:v hevc_nvenc", "-rc:v vbr -cq:v 20 -b:v 0", "-preset p7"} {
		if !strings.Contains(joined, piece) {
			t.Errorf("archivist nvenc (unverified tuning) argv missing %q: %q", piece, joined)
		}
	}
	for _, stripped := range []string{"-tune", "-multipass", "-spatial-aq", "-temporal-aq", "-b_ref_mode"} {
		if strings.Contains(joined, stripped) {
			t.Errorf("archivist nvenc (unverified tuning) must strip %q: %q", stripped, joined)
		}
	}
}

// TestBuildFfmpegArgsArchivistNVENCAV1 covers the Archivist NVIDIA AV1 path:
// constant quality 24 via VBR with zero target bitrate, the slowest speed
// preset (p7), spatial AQ, multipass fullres, middle B-frame reference mode,
// and temporal-aq omitted.
func TestBuildFfmpegArgsArchivistNVENCAV1(t *testing.T) {
	caps := EncoderCaps{
		HasNvencAV1: true, HasNvencAV1Verified: true, HasNvencAV1TuningVerified: true,
		RecommendedEncoder: "av1_nvenc",
	}
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "av1",
		Spec:       archivistJobSpec("nvenc"),
		Caps:       &caps,
	}
	want := append(append(append([]string{},
		archivistArgvPrefix...),
		"-c:v", "av1_nvenc", "-rc:v", "vbr", "-cq:v", "24", "-b:v", "0",
		"-pix_fmt", "p010le",
		"-tune", "hq", "-multipass", "fullres", "-spatial-aq", "1", "-b_ref_mode", "middle",
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709",
		"-preset", "p7"),
		archivistArgvSuffix...)

	got, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("archivist av1_nvenc argv mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestBuildFfmpegArgsArchivistQSV covers the Archivist Intel path: ICQ
// quality 13, the slower speed preset, extended bitrate control, and
// adaptive I/B frames — with no lookahead, which the driver silently
// ignores under constant-quality rate control.
func TestBuildFfmpegArgsArchivistQSV(t *testing.T) {
	caps := EncoderCaps{
		HasIntel: true, HasQSVHEVC: true,
		HasQSVExtBRCVerified: true, HasQSVAdaptiveIBVerified: true,
		RecommendedEncoder: "hevc_qsv",
	}
	want := append(append(append([]string{},
		archivistArgvPrefix...),
		"-c:v", "hevc_qsv", "-rc", "icq", "-global_quality", "13",
		"-profile:v", "main10", "-pix_fmt", "p010le",
		"-g", "240",
		"-extbrc", "1", "-adaptive_i", "1", "-adaptive_b", "1",
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709",
		"-preset", "slower"),
		archivistArgvSuffix...)

	got, err := buildFfmpegArgs(archivistJob("qsv", caps), nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("archivist qsv argv mismatch:\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(strings.Join(got, " "), "look_ahead") {
		t.Errorf("archivist qsv package must not include lookahead: %q", got)
	}
}

// TestBuildFfmpegArgsArchivistQSVStripsUnverifiedFlags proves each QSV
// tuning flag group is stripped independently: adaptive I/B go when only
// extbrc was canary-proven, and everything goes when nothing was proven —
// while ICQ 13 and slower hold at every step.
func TestBuildFfmpegArgsArchivistQSVStripsUnverifiedFlags(t *testing.T) {
	base := EncoderCaps{HasIntel: true, HasQSVHEVC: true, RecommendedEncoder: "hevc_qsv"}

	t.Run("extbrc only", func(t *testing.T) {
		caps := base
		caps.HasQSVExtBRCVerified = true
		got, err := buildFfmpegArgs(archivistJob("qsv", caps), nil)
		if err != nil {
			t.Fatalf("buildFfmpegArgs: %v", err)
		}
		joined := strings.Join(got, " ")
		for _, piece := range []string{"-rc icq -global_quality 13", "-extbrc 1", "-preset slower"} {
			if !strings.Contains(joined, piece) {
				t.Errorf("argv missing %q: %q", piece, joined)
			}
		}
		for _, stripped := range []string{"-adaptive_i", "-adaptive_b", "look_ahead"} {
			if strings.Contains(joined, stripped) {
				t.Errorf("adaptive-flag canary failed; must strip %q: %q", stripped, joined)
			}
		}
	})

	t.Run("nothing verified", func(t *testing.T) {
		got, err := buildFfmpegArgs(archivistJob("qsv", base), nil)
		if err != nil {
			t.Fatalf("buildFfmpegArgs: %v", err)
		}
		joined := strings.Join(got, " ")
		for _, piece := range []string{"-rc icq -global_quality 13", "-preset slower"} {
			if !strings.Contains(joined, piece) {
				t.Errorf("argv missing %q: %q", piece, joined)
			}
		}
		for _, stripped := range []string{"-extbrc", "-adaptive_i", "-adaptive_b", "look_ahead"} {
			if strings.Contains(joined, stripped) {
				t.Errorf("no QSV canary passed; must strip %q: %q", stripped, joined)
			}
		}
	})
}

// TestBuildFfmpegArgsArchivistCPU covers the software fallback: exactly the
// historical software Archivist (CRF 18, slow, aq-mode 3, SAO off, tune
// film) — owned in code, overriding whatever tuning fields the preset row
// carried.
func TestBuildFfmpegArgsArchivistCPU(t *testing.T) {
	caps := EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}
	spec := archivistJobSpec("cpu")
	spec.AqMode = 2
	spec.FilmGrainSao = true
	spec.ContentTuning = "grain"
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "hevc",
		Spec:       spec,
		Caps:       &caps,
	}
	want := append(append(append([]string{},
		archivistArgvPrefix...),
		"-c:v", "libx265", "-crf", "18",
		"-pix_fmt", "yuv420p10le",
		"-preset", "slow",
		"-x265-params", "aq-mode=3:sao=0:tune=film"),
		archivistArgvSuffix...)

	got, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("archivist cpu argv mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestBuildFfmpegArgsArchivistFallbackChain proves the NVENC → QSV stepdown
// stays at Archivist grade: the server-assigned nvenc engine is unavailable
// on this machine, resolution falls back to QSV, and the QSV package (not
// the preset's plain values) is applied.
func TestBuildFfmpegArgsArchivistFallbackChain(t *testing.T) {
	caps := EncoderCaps{
		HasIntel: true, HasQSVHEVC: true,
		HasQSVExtBRCVerified: true, HasQSVAdaptiveIBVerified: true,
		RecommendedEncoder: "hevc_qsv",
	}
	got, err := buildFfmpegArgs(archivistJob("nvenc", caps), nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	joined := strings.Join(got, " ")
	for _, piece := range []string{
		"-c:v hevc_qsv",
		"-rc icq -global_quality 13",
		"-extbrc 1 -adaptive_i 1 -adaptive_b 1",
		"-preset slower",
	} {
		if !strings.Contains(joined, piece) {
			t.Errorf("archivist nvenc→qsv fallback argv missing %q: %q", piece, joined)
		}
	}
	if strings.Contains(joined, "hevc_nvenc") || strings.Contains(joined, "-cq:v") {
		t.Errorf("archivist fallback must not emit the unavailable nvenc package: %q", joined)
	}
}

// TestBuildFfmpegArgsArchivistFallbackToCPU proves the QSV → CPU stepdown
// stays at Archivist grade: the software Archivist package (CRF 18, slow,
// aq-mode 3, SAO off, tune film) replaces the preset's plain values.
func TestBuildFfmpegArgsArchivistFallbackToCPU(t *testing.T) {
	caps := EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}
	got, err := buildFfmpegArgs(archivistJob("qsv", caps), nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	joined := strings.Join(got, " ")
	for _, piece := range []string{
		"-c:v libx265",
		"-crf 18",
		"-preset slow",
		"-x265-params aq-mode=3:sao=0:tune=film",
	} {
		if !strings.Contains(joined, piece) {
			t.Errorf("archivist qsv→cpu fallback argv missing %q: %q", piece, joined)
		}
	}
	for _, stripped := range []string{"-global_quality", "-extbrc", "-adaptive_i", "hevc_qsv"} {
		if strings.Contains(joined, stripped) {
			t.Errorf("archivist qsv→cpu fallback must not emit the unavailable qsv package (%q): %q", stripped, joined)
		}
	}
}

// TestArchivistQSVSlowerAccepted locks the acceptance criterion that
// slower is a valid speed setting for the QSV Archivist package.
func TestArchivistQSVSlowerAccepted(t *testing.T) {
	if !isValidPreset("slower") {
		t.Error("isValidPreset must accept 'slower' for the Archivist QSV package")
	}
}

// TestVerifyArchivistQSVFlagsCanaries proves the two-tier canary: a driver
// accepting the full package verifies both flag groups; a driver that
// rejects adaptive I/B frames still verifies (and keeps) extbrc.
func TestVerifyArchivistQSVFlagsCanaries(t *testing.T) {
	tmp := t.TempDir()
	pass := fakeTool(t, tmp, "ffmpeg-pass", "#!/bin/sh\nexit 0\n")
	fullCaps := EncoderCaps{HasQSVHEVC: true}
	verifyArchivistQSVFlags(pass, &fullCaps)
	if !fullCaps.HasQSVExtBRCVerified || !fullCaps.HasQSVAdaptiveIBVerified {
		t.Errorf("full-package canary pass must verify both flag groups: %+v", fullCaps)
	}

	noAdaptive := fakeTool(t, tmp, "ffmpeg-noadaptive", `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "-adaptive_i" ]; then exit 1; fi
done
exit 0
`)
	partialCaps := EncoderCaps{HasQSVHEVC: true}
	verifyArchivistQSVFlags(noAdaptive, &partialCaps)
	if !partialCaps.HasQSVExtBRCVerified {
		t.Error("extbrc-only retry must verify extbrc when adaptive flags are rejected")
	}
	if partialCaps.HasQSVAdaptiveIBVerified {
		t.Error("adaptive I/B must stay unverified when the driver rejects them")
	}

	// A driver that rejects extbrc but accepts adaptive I/B frames keeps
	// the adaptive group alone — each flag group is verified per driver.
	noExtbrc := fakeTool(t, tmp, "ffmpeg-noextbrc", `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "-extbrc" ]; then exit 1; fi
done
exit 0
`)
	mirrorCaps := EncoderCaps{HasQSVHEVC: true}
	verifyArchivistQSVFlags(noExtbrc, &mirrorCaps)
	if mirrorCaps.HasQSVExtBRCVerified {
		t.Error("extbrc must stay unverified when the driver rejects it")
	}
	if !mirrorCaps.HasQSVAdaptiveIBVerified {
		t.Error("adaptive-only retry must verify adaptive I/B when the driver accepts them")
	}

	none := fakeTool(t, tmp, "ffmpeg-fail", "#!/bin/sh\nexit 1\n")
	failCaps := EncoderCaps{HasQSVHEVC: true}
	verifyArchivistQSVFlags(none, &failCaps)
	if failCaps.HasQSVExtBRCVerified || failCaps.HasQSVAdaptiveIBVerified {
		t.Errorf("failing canaries must verify nothing: %+v", failCaps)
	}

	// No QSV HEVC encoder at all: no canaries run.
	skipCaps := EncoderCaps{}
	verifyArchivistQSVFlags(pass, &skipCaps)
	if skipCaps.HasQSVExtBRCVerified || skipCaps.HasQSVAdaptiveIBVerified {
		t.Errorf("canaries must not run without a QSV HEVC encoder: %+v", skipCaps)
	}
}

// TestBuildFfmpegArgsArchivistLibsvtav1 covers the Archivist SVT-AV1 path:
// CRF 22, preset 4, 10-bit yuv420p10le, and tune=0:film-grain=8.
func TestBuildFfmpegArgsArchivistLibsvtav1(t *testing.T) {
	caps := EncoderCaps{HasLibsvtAV1: true, RecommendedEncoder: "libsvtav1"}
	spec := archivistJobSpec("cpu")
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "libsvtav1",
		Spec:       spec,
		Caps:       &caps,
	}
	want := append(append(append([]string{},
		archivistArgvPrefix...),
		"-c:v", "libsvtav1", "-crf", "22", "-pix_fmt", "yuv420p10le",
		"-preset", "4", "-svtav1-params", "tune=0:film-grain=8"),
		archivistArgvSuffix...)

	got, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv mismatch:\n got %v\nwant %v", got, want)
	}
}

// TestBuildFfmpegArgsSvtav1Tiers covers Balanced and Space Savings software AV1 presets.
func TestBuildFfmpegArgsSvtav1Tiers(t *testing.T) {
	caps := EncoderCaps{HasLibsvtAV1: true, RecommendedEncoder: "libsvtav1"}

	// Balanced: CRF 26, preset 6, film-grain=4
	balancedSpec := jobspec.JobSpec{
		Container:        "mkv",
		EncoderEngine:    "cpu",
		CRF:              26,
		PresetSpeed:      "6",
		QualityTarget:    26,
		AudioHandling:    "copy_all",
		SubtitleHandling: "copy_all",
		ColorDepth:       "10bit",
		ContentTuning:    "film",
	}
	balancedJob := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "libsvtav1",
		Spec:       balancedSpec,
		Caps:       &caps,
	}
	balancedArgs, err := buildFfmpegArgs(balancedJob, nil)
	if err != nil {
		t.Fatalf("balanced job: %v", err)
	}
	joinedBalanced := strings.Join(balancedArgs, " ")
	if !strings.Contains(joinedBalanced, "-c:v libsvtav1") ||
		!strings.Contains(joinedBalanced, "-crf 26") ||
		!strings.Contains(joinedBalanced, "-preset 6") ||
		!strings.Contains(joinedBalanced, "-svtav1-params tune=0:film-grain=4") {
		t.Errorf("balanced argv mismatch: %v", balancedArgs)
	}

	// Space Savings: CRF 32, preset 8, film-grain=0
	savingsSpec := jobspec.JobSpec{
		Container:        "mkv",
		EncoderEngine:    "cpu",
		CRF:              32,
		PresetSpeed:      "8",
		QualityTarget:    32,
		AudioHandling:    "copy_all",
		SubtitleHandling: "copy_all",
		ColorDepth:       "10bit",
		ContentTuning:    "none",
	}
	savingsJob := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "libsvtav1",
		Spec:       savingsSpec,
		Caps:       &caps,
	}
	savingsArgs, err := buildFfmpegArgs(savingsJob, nil)
	if err != nil {
		t.Fatalf("savings job: %v", err)
	}
	joinedSavings := strings.Join(savingsArgs, " ")
	if !strings.Contains(joinedSavings, "-c:v libsvtav1") ||
		!strings.Contains(joinedSavings, "-crf 32") ||
		!strings.Contains(joinedSavings, "-preset 8") {
		t.Errorf("savings argv mismatch: %v", savingsArgs)
	}
}
