package ffmpeg

import (
	"reflect"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func nvencTestJob(vt *protocol.VendorTuning, caps EncoderCaps) TranscodeJob {
	return TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mp4",
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Container:        "mp4",
			EncoderEngine:    "nvenc",
			QualityTarget:    22,
			AudioHandling:    "copy_all",
			SubtitleHandling: "copy_all",
			ColorDepth:       "8bit",
			VendorTuning:     vt,
		},
		Caps: &caps,
	}
}

func qsvTestJob(codec string, vt *protocol.VendorTuning, caps EncoderCaps) TranscodeJob {
	return TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mp4",
		Codec:      codec,
		Spec: jobspec.JobSpec{
			Container:        "mp4",
			EncoderEngine:    "qsv",
			QualityTarget:    22,
			AudioHandling:    "copy_all",
			SubtitleHandling: "copy_all",
			ColorDepth:       "8bit",
			VendorTuning:     vt,
		},
		Caps: &caps,
	}
}

func vendorArgsFromBuild(t *testing.T, job TranscodeJob) []string {
	t.Helper()
	args, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	// Extract the vendor tuning argv slice: everything from the first vendor
	// flag up to (excluding) "-preset".
	start := -1
	for i, a := range args {
		if a == "-tune" || a == "-extbrc" {
			start = i
			break
		}
	}
	if start == -1 {
		return nil
	}
	var out []string
	for _, a := range args[start:] {
		if a == "-preset" {
			break
		}
		out = append(out, a)
	}
	return out
}

func TestVendorTuningUserSelectionNvenc(t *testing.T) {
	caps := EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: true, RecommendedEncoder: "hevc_nvenc"}

	// Explicit selection equal to the defaults renders the default package.
	full := &protocol.VendorTuning{
		Tune:       strPtr("hq"),
		Multipass:  strPtr("fullres"),
		SpatialAQ:  boolPtr(true),
		TemporalAQ: boolPtr(true),
		BRefMode:   strPtr("middle"),
	}
	wantDefault := []string{"-tune", "hq", "-multipass", "fullres", "-spatial-aq", "1", "-temporal-aq", "1", "-b_ref_mode", "middle"}
	if got := vendorArgsFromBuild(t, nvencTestJob(full, caps)); !reflect.DeepEqual(got, wantDefault) {
		t.Errorf("explicit-default selection mismatch:\n got: %q\nwant: %q", got, wantDefault)
	}

	// Partial selection: unset fields fall back to defaults, explicit false
	// omits the flag.
	partial := &protocol.VendorTuning{
		Tune:      strPtr("ll"),
		SpatialAQ: boolPtr(false),
	}
	wantPartial := []string{"-tune", "ll", "-multipass", "fullres", "-temporal-aq", "1", "-b_ref_mode", "middle"}
	if got := vendorArgsFromBuild(t, nvencTestJob(partial, caps)); !reflect.DeepEqual(got, wantPartial) {
		t.Errorf("partial selection mismatch:\n got: %q\nwant: %q", got, wantPartial)
	}
}

func TestVendorTuningUserSelectionQsv(t *testing.T) {
	caps := EncoderCaps{HasQSVHEVC: true, HasQSVHEVCTuningVerified: true, RecommendedEncoder: "hevc_qsv"}

	// Look-ahead off drops both look_ahead flags; depth follows selection.
	vt := &protocol.VendorTuning{
		LookAhead:      boolPtr(false),
		LookAheadDepth: intPtr(60),
	}
	want := []string{"-extbrc", "1", "-b_strategy", "1", "-adaptive_b", "1", "-adaptive_i", "1"}
	if got := vendorArgsFromBuild(t, qsvTestJob("hevc", vt, caps)); !reflect.DeepEqual(got, want) {
		t.Errorf("qsv look-ahead-off mismatch:\n got: %q\nwant: %q", got, want)
	}

	vt = &protocol.VendorTuning{LookAheadDepth: intPtr(60)}
	want = []string{"-extbrc", "1", "-look_ahead", "1", "-look_ahead_depth", "60", "-b_strategy", "1", "-adaptive_b", "1", "-adaptive_i", "1"}
	if got := vendorArgsFromBuild(t, qsvTestJob("hevc", vt, caps)); !reflect.DeepEqual(got, want) {
		t.Errorf("qsv depth-60 mismatch:\n got: %q\nwant: %q", got, want)
	}

	// h264_qsv accepts only the reduced set: adaptive selections are ignored.
	h264Caps := EncoderCaps{HasQSVH264: true, HasQSVH264TuningVerified: true, RecommendedEncoder: "h264_qsv"}
	vt = &protocol.VendorTuning{
		AdaptiveB: boolPtr(true),
		LookAhead: boolPtr(true),
	}
	want = []string{"-extbrc", "1", "-b_strategy", "1"}
	if got := vendorArgsFromBuild(t, qsvTestJob("h264", vt, h264Caps)); !reflect.DeepEqual(got, want) {
		t.Errorf("h264_qsv reduced set mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestVendorTuningCanaryFailureStripsSelection(t *testing.T) {
	// Driver canary failed: user selection is dropped like the default
	// package, never emitted unchecked.
	caps := EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: false, RecommendedEncoder: "hevc_nvenc"}
	vt := &protocol.VendorTuning{Tune: strPtr("ll")}
	args, err := buildFfmpegArgs(nvencTestJob(vt, caps), nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	for i, a := range args {
		if a == "-tune" && i+1 < len(args) && args[i+1] == "ll" {
			t.Fatalf("user tuning emitted despite failed canary: %q", args)
		}
	}
}

func TestVendorTuningArchivistStillCodeOwned(t *testing.T) {
	caps := EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: true, RecommendedEncoder: "hevc_nvenc"}
	job := nvencTestJob(&protocol.VendorTuning{Tune: strPtr("ll"), SpatialAQ: boolPtr(false)}, caps)
	job.Spec.QualityTier = protocol.QualityTierArchivist
	args, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-tune ll") {
		t.Fatalf("archivist grade must keep the code-owned package; user tuning leaked: %q", args)
	}
	// Archivist NVENC package: CQ 20 at p7 with the hq tuning set.
	for _, tok := range []string{"-cq:v", "20", "-preset", "p7", "-tune", "hq"} {
		if !strings.Contains(joined, tok) {
			t.Errorf("archivist package missing %q: %q", tok, args)
		}
	}
}

func TestVendorTuningIgnoredWithoutEncoderPackage(t *testing.T) {
	// h264_nvenc has no vendor package: the selection is meaningless and the
	// driver defaults apply.
	caps := EncoderCaps{HasNvencH264: true, RecommendedEncoder: "h264_nvenc"}
	job := nvencTestJob(&protocol.VendorTuning{Tune: strPtr("ll")}, caps)
	job.Codec = "h264"
	args, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	for _, a := range args {
		if a == "-tune" {
			t.Fatalf("h264_nvenc must not emit vendor tuning: %q", args)
		}
	}
}

func TestVendorTuningArgsForNil(t *testing.T) {
	if got := vendorTuningArgsFor("hevc_nvenc", nil); got != nil {
		t.Errorf("nil selection must render nil argv, got %q", got)
	}
	if got := vendorTuningArgsFor("libx265", &protocol.VendorTuning{Tune: strPtr("hq")}); got != nil {
		t.Errorf("software encoder must render nil argv, got %q", got)
	}
}

func TestVendorTuningAmfFallbackKeepsDefaultPackage(t *testing.T) {
	// A preset carrying NVENC/QSV tuning can resolve to hevc_amf on an AMD
	// fallback agent: the selection cannot map, so the canary-verified AMF
	// default package must apply unchanged (never wiped to nothing).
	caps := EncoderCaps{HasAMFHEVC: true, HasAMFTuningVerified: true, RecommendedEncoder: "hevc_amf"}
	job := nvencTestJob(&protocol.VendorTuning{Tune: strPtr("ll")}, caps)
	job.Spec.EncoderEngine = "amf"
	args, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, tok := range []string{"-quality", "quality", "-b_frame_delta_qp", "4", "-vbaq", "true"} {
		if !strings.Contains(joined, tok) {
			t.Errorf("amf default package missing %q: %q", tok, args)
		}
	}
	if strings.Contains(joined, "-tune ll") {
		t.Errorf("foreign nvenc selection leaked into amf argv: %q", args)
	}
}

func TestVendorTuningCrossFamilySelectionAppliesResolvedDefaults(t *testing.T) {
	// An NVENC preset resolved to QSV (fallback agent): the NVENC selection
	// is foreign; QSV defaults apply, not a partial mix.
	caps := EncoderCaps{HasQSVHEVC: true, HasQSVHEVCTuningVerified: true, RecommendedEncoder: "hevc_qsv"}
	job := nvencTestJob(&protocol.VendorTuning{Tune: strPtr("ll")}, caps)
	job.Spec.EncoderEngine = "qsv"
	got := vendorArgsFromBuild(t, job)
	want := []string{"-extbrc", "1", "-look_ahead", "1", "-look_ahead_depth", "40", "-b_strategy", "1", "-adaptive_b", "1", "-adaptive_i", "1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cross-family fallback mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestVendorTuningDryRunValidation(t *testing.T) {
	tmp := t.TempDir()
	passFFmpeg := fakeTool(t, tmp, "ffmpeg-vt-pass", "#!/bin/sh\nexit 0\n")
	caps := EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: true, RecommendedEncoder: "hevc_nvenc"}
	job := nvencTestJob(&protocol.VendorTuning{Tune: strPtr("hq")}, caps)
	job.FFmpegPath = passFFmpeg

	_, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("expected buildFfmpegArgs to succeed with passing dry-run, got: %v", err)
	}

	failFFmpeg := fakeTool(t, tmp, "ffmpeg-vt-fail", "#!/bin/sh\necho \"Unknown option -bad_flag\" >&2\nexit 1\n")
	job.FFmpegPath = failFFmpeg
	_, err = buildFfmpegArgs(job, nil)
	if err == nil {
		t.Fatalf("expected buildFfmpegArgs to fail when dry-run rejects tuning flags")
	}
	if !strings.Contains(err.Error(), "vendor hardware tuning parameters invalid") {
		t.Errorf("expected error to mention vendor hardware tuning parameters invalid, got: %v", err)
	}
}
