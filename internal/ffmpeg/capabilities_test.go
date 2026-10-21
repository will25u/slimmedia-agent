package ffmpeg

import (
	"path/filepath"
	"testing"
)

// encoderListLine mimics one line of `ffmpeg -encoders` output: leading space,
// codec flags, a space, then the encoder name followed by a space.
const encoderListHeader = " ------\n"

func encoderList(names ...string) string {
	s := encoderListHeader
	for _, n := range names {
		s += " V..... " + n + "            description\n"
	}
	return s
}

func TestParseEncoderCapsHardwareAV1(t *testing.T) {
	tests := []struct {
		name     string
		encoders string
		want     EncoderCaps
	}{
		{
			name:     "nvenc av1 only",
			encoders: encoderList("av1_nvenc"),
			want:     EncoderCaps{HasNvencAV1: true},
		},
		{
			name:     "qsv av1 only",
			encoders: encoderList("av1_qsv"),
			want:     EncoderCaps{HasQSVAV1: true},
		},
		{
			name:     "amf av1 only",
			encoders: encoderList("av1_amf"),
			want:     EncoderCaps{HasAMFAV1: true},
		},
		{
			name: "all av1 hardware encoders",
			encoders: encoderList(
				"av1_nvenc", "av1_qsv", "av1_amf",
				"h264_nvenc", "hevc_nvenc", "h264_qsv", "hevc_qsv", "h264_amf", "hevc_amf",
				"libx264", "libx265", "libaom-av1", "libsvtav1",
				"h264_videotoolbox", "hevc_videotoolbox",
			),
			want: EncoderCaps{
				HasNvencH264: true, HasNvencHEVC: true, HasNvencAV1: true,
				HasQSVH264: true, HasQSVHEVC: true, HasQSVAV1: true,
				HasAMFH264: true, HasAMFHEVC: true, HasAMFAV1: true,
				HasLibx264: true, HasLibx265: true, HasLibaomAV1: true, HasLibsvtAV1: true,
				HasVTH264: true, HasVTHEVC: true,
			},
		},
		{
			name:     "cpu-only build has no av1 hardware flags",
			encoders: encoderList("libx264", "libx265", "libaom-av1", "libsvtav1"),
			want: EncoderCaps{
				HasLibx264: true, HasLibx265: true, HasLibaomAV1: true, HasLibsvtAV1: true,
			},
		},
		{
			name:     "empty encoder list",
			encoders: encoderListHeader,
			want:     EncoderCaps{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseEncoderCaps(tt.encoders)
			if got != tt.want {
				t.Errorf("parseEncoderCaps() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseEncoderCapsSubstringSafety(t *testing.T) {
	// "av1_nvenc" as a bare substring of a longer name must not trip detection:
	// matching is anchored on surrounding spaces, so a name like
	// "av1_nvenc_foobar" (no trailing space before description) is still caught,
	// but unrelated tokens are not.
	got := parseEncoderCaps(encoderList("libx264", "hevc_amf"))
	if !got.HasAMFHEVC || got.HasAMFAV1 {
		t.Errorf("parseEncoderCaps() = %+v, want HasAMFHEVC only", got)
	}
}

func TestCanaryEncodeMissingBinary(t *testing.T) {
	if canaryEncode(filepath.Join(t.TempDir(), "no-such-ffmpeg"), "av1_qsv") {
		t.Error("canaryEncode with missing binary = true, want false")
	}
}

func TestAV1CanaryArgsCoverAllEncoders(t *testing.T) {
	for _, encoder := range []string{"av1_nvenc", "av1_qsv", "av1_amf"} {
		if len(av1CanaryArgs[encoder]) == 0 {
			t.Errorf("av1CanaryArgs has no quality args for %s", encoder)
		}
	}
}

func TestHWTuningFlagSetsCoverage(t *testing.T) {
	for _, encoder := range []string{"hevc_nvenc", "av1_nvenc", "hevc_qsv", "h264_qsv", "av1_qsv", "hevc_amf"} {
		flags, ok := hwTuningFlagSets[encoder]
		if !ok || len(flags) == 0 {
			t.Errorf("hwTuningFlagSets missing or empty for %s", encoder)
		}
	}

	// Verify NVENC has multipass fullres
	for _, nvencEnc := range []string{"hevc_nvenc", "av1_nvenc"} {
		nvencFlags := hwTuningFlagSets[nvencEnc]
		hasMultipass := false
		for i := 0; i < len(nvencFlags)-1; i++ {
			if nvencFlags[i] == "-multipass" && nvencFlags[i+1] == "fullres" {
				hasMultipass = true
				break
			}
		}
		if !hasMultipass {
			t.Errorf("%s tuning set must include -multipass fullres: %v", nvencEnc, nvencFlags)
		}
	}

	// Verify av1_nvenc omits temporal-aq per community consensus
	for _, flag := range hwTuningFlagSets["av1_nvenc"] {
		if flag == "-temporal-aq" || flag == "-temporal_aq" {
			t.Errorf("av1_nvenc tuning set must not include temporal-aq: %v", hwTuningFlagSets["av1_nvenc"])
		}
	}

	// Verify QSV H264 and AV1 have -extbrc 1 and -b_strategy 1
	for _, qsvEnc := range []string{"h264_qsv", "av1_qsv"} {
		flags := hwTuningFlagSets[qsvEnc]
		hasExtbrc, hasBStrategy := false, false
		for i := 0; i < len(flags)-1; i++ {
			if flags[i] == "-extbrc" && flags[i+1] == "1" {
				hasExtbrc = true
			}
			if flags[i] == "-b_strategy" && flags[i+1] == "1" {
				hasBStrategy = true
			}
		}
		if !hasExtbrc || !hasBStrategy {
			t.Errorf("%s tuning set must include -extbrc 1 and -b_strategy 1: %v", qsvEnc, flags)
		}
	}
}

func TestVerifyTuningFlagsCanaries(t *testing.T) {
	tmp := t.TempDir()
	// Fake ffmpeg script that exits 0
	passFFmpeg := fakeTool(t, tmp, "ffmpeg-pass", "#!/bin/sh\nexit 0\n")
	caps := EncoderCaps{
		HasNvencHEVC: true,
		HasNvencAV1:  true,
		HasQSVHEVC:   true,
		HasQSVH264:   true,
		HasQSVAV1:    true,
		HasAMFHEVC:   true,
	}
	verifyTuningFlags(passFFmpeg, &caps)
	if !caps.HasNvencTuningVerified {
		t.Error("expected HasNvencTuningVerified to be true")
	}
	if !caps.HasNvencAV1TuningVerified {
		t.Error("expected HasNvencAV1TuningVerified to be true")
	}
	if !caps.HasQSVHEVCTuningVerified {
		t.Error("expected HasQSVHEVCTuningVerified to be true")
	}
	if !caps.HasQSVH264TuningVerified {
		t.Error("expected HasQSVH264TuningVerified to be true")
	}
	if !caps.HasQSVAV1TuningVerified {
		t.Error("expected HasQSVAV1TuningVerified to be true")
	}
	if !caps.HasAMFTuningVerified {
		t.Error("expected HasAMFTuningVerified to be true")
	}

	// Fake ffmpeg script that exits 1 (unsupported / driver failure)
	failFFmpeg := fakeTool(t, tmp, "ffmpeg-fail", "#!/bin/sh\nexit 1\n")
	failCaps := EncoderCaps{
		HasNvencHEVC: true,
		HasNvencAV1:  true,
		HasQSVHEVC:   true,
		HasQSVH264:   true,
		HasQSVAV1:    true,
		HasAMFHEVC:   true,
	}
	verifyTuningFlags(failFFmpeg, &failCaps)
	if failCaps.HasNvencTuningVerified || failCaps.HasNvencAV1TuningVerified || failCaps.HasQSVHEVCTuningVerified ||
		failCaps.HasQSVH264TuningVerified || failCaps.HasQSVAV1TuningVerified || failCaps.HasAMFTuningVerified {
		t.Errorf("failing canary must not verify any tuning flags: %+v", failCaps)
	}
}

func TestVerifyQSVEncodersCanaries(t *testing.T) {
	tmp := t.TempDir()
	passFFmpeg := fakeTool(t, tmp, "ffmpeg-qsv-pass", "#!/bin/sh\nexit 0\n")
	caps := EncoderCaps{
		HasQSVHEVC: true,
		HasQSVH264: true,
	}
	verifyQSVEncoders(passFFmpeg, &caps)
	if !caps.HasQSVHEVCVerified || !caps.HasQSVH264Verified {
		t.Errorf("expected HasQSVHEVCVerified and HasQSVH264Verified to be true on pass, got: %+v", caps)
	}

	failFFmpeg := fakeTool(t, tmp, "ffmpeg-qsv-fail", "#!/bin/sh\nexit 1\n")
	failCaps := EncoderCaps{
		HasQSVHEVC: true,
		HasQSVH264: true,
	}
	verifyQSVEncoders(failFFmpeg, &failCaps)
	if failCaps.HasQSVHEVCVerified || failCaps.HasQSVH264Verified {
		t.Errorf("expected failing canary to keep QSV flags unverified, got: %+v", failCaps)
	}
}

func TestParseHWAccels(t *testing.T) {
	out := `Hardware acceleration methods:
vdpau
cuda
vaapi
qsv
d3d11va
opencl
`
	cuda, vaapi, d3d11va, qsv := parseHWAccels(out)
	if !cuda || !vaapi || !d3d11va || !qsv {
		t.Errorf("parseHWAccels() = cuda:%v, vaapi:%v, d3d11va:%v, qsv:%v; want all true", cuda, vaapi, d3d11va, qsv)
	}

	cuda2, vaapi2, d3d11va2, qsv2 := parseHWAccels("Hardware acceleration methods:\nopencl\nvulkan\n")
	if cuda2 || vaapi2 || d3d11va2 || qsv2 {
		t.Errorf("parseHWAccels() on empty backends = cuda:%v, vaapi:%v, d3d11va:%v, qsv:%v; want all false", cuda2, vaapi2, d3d11va2, qsv2)
	}
}

func TestVerifyHWDecodersCanaries(t *testing.T) {
	tmp := t.TempDir()
	passFFmpeg := fakeTool(t, tmp, "ffmpeg-hwdec-pass", "#!/bin/sh\nexit 0\n")
	caps := EncoderCaps{
		HasHWAccelCUDA: true,
	}
	verifyHWDecoders(passFFmpeg, &caps)
	if !caps.HasHWDecodeHEVC10Verified {
		t.Error("expected HasHWDecodeHEVC10Verified to be true")
	}
	if !caps.HasHWDecodeAV1Verified {
		t.Error("expected HasHWDecodeAV1Verified to be true")
	}

	failFFmpeg := fakeTool(t, tmp, "ffmpeg-hwdec-fail", "#!/bin/sh\nexit 1\n")
	failCaps := EncoderCaps{
		HasHWAccelCUDA: true,
	}
	verifyHWDecoders(failFFmpeg, &failCaps)
	if failCaps.HasHWDecodeHEVC10Verified || failCaps.HasHWDecodeAV1Verified {
		t.Errorf("failing canary must not verify HW decoders: %+v", failCaps)
	}
}

func TestVerifyHWTonemapCanaries(t *testing.T) {
	tmp := t.TempDir()
	passFFmpeg := fakeTool(t, tmp, "ffmpeg-tonemap-pass", "#!/bin/sh\nexit 0\n")
	caps := EncoderCaps{
		HasHWAccelCUDA:  true,
		HasHWAccelVAAPI: true,
	}
	verifyHWTonemapFilters(passFFmpeg, &caps)
	if !caps.HasHWTonemapVerified {
		t.Error("expected HasHWTonemapVerified to be true")
	}
	if !caps.HasTonemapOpenCLVerified || !caps.HasTonemapCUDAVerified || !caps.HasTonemapVAAPIVerified {
		t.Errorf("expected all tone-mapping flags true on pass, got: %+v", caps)
	}

	failFFmpeg := fakeTool(t, tmp, "ffmpeg-tonemap-fail", "#!/bin/sh\nexit 1\n")
	failCaps := EncoderCaps{
		HasHWAccelCUDA:  true,
		HasHWAccelVAAPI: true,
	}
	verifyHWTonemapFilters(failFFmpeg, &failCaps)
	if failCaps.HasHWTonemapVerified || failCaps.HasTonemapOpenCLVerified || failCaps.HasTonemapCUDAVerified || failCaps.HasTonemapVAAPIVerified {
		t.Errorf("failing canary must not verify tonemap filters: %+v", failCaps)
	}
}

func TestStripUnverifiedAndDeriveRecommendation(t *testing.T) {
	// Scenario: static FFmpeg reports nvenc and cuda, but machine has Intel QSV (verified) and no Nvidia hardware.
	caps := EncoderCaps{
		HasLibx264:               true,
		HasLibx265:               true,
		HasNvencH264:             true,
		HasNvencHEVC:             true,
		HasHWAccelCUDA:           true,
		HasQSVH264:               true,
		HasQSVHEVC:               true,
		HasQSVHEVCTuningVerified: true,
	}

	// Before stripping, deriveRecommendation with no hardware and unverified nvenc must pick QSV
	enc, preset, crf, _ := deriveRecommendation(caps)
	if enc != "hevc_qsv" {
		t.Errorf("deriveRecommendation() = %q, want hevc_qsv (NVENC unverified without hardware)", enc)
	}
	if preset != "medium" || crf != 25 {
		t.Errorf("deriveRecommendation() preset=%q crf=%d, want medium / 25", preset, crf)
	}

	// Now strip unverified: machine has hasNvidia=false, hasIntel=true, hasAMD=true
	caps.StripUnverified(false, true, true)

	if caps.HasNvencHEVC || caps.HasNvencH264 || caps.HasHWAccelCUDA {
		t.Errorf("NVENC/CUDA flags must be stripped when unverified and no Nvidia hardware: %+v", caps)
	}
	if !caps.HasQSVHEVC || !caps.HasQSVH264 {
		t.Errorf("QSV flags must be preserved when verified / hardware present: %+v", caps)
	}
	if caps.RecommendedEncoder != "hevc_qsv" {
		t.Errorf("RecommendedEncoder = %q, want hevc_qsv", caps.RecommendedEncoder)
	}
}

