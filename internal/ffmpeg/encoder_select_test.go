package ffmpeg

import "testing"

func TestResolveEncoder(t *testing.T) {
	cpuCaps := EncoderCaps{HasLibx265: true, HasLibx264: true, HasLibaomAV1: true, HasLibsvtAV1: true, RecommendedEncoder: "libx265"}
	nvencCaps := EncoderCaps{
		HasNvencHEVC: true, HasNvencH264: true,
		HasNvencAV1: true, HasNvencAV1Verified: true,
		RecommendedEncoder: "hevc_nvenc",
	}
	qsvCaps := EncoderCaps{HasIntel: true, HasQSVHEVC: true, HasQSVH264: true, HasQSVHEVCVerified: true, HasQSVH264Verified: true, RecommendedEncoder: "hevc_qsv"}

	tests := []struct {
		name        string
		requested   string
		targetCodec string
		caps        EncoderCaps
		want        string
	}{
		{"empty request keeps target codec", "", "hevc", cpuCaps, "hevc"},
		{"cpu hevc", "cpu", "hevc", cpuCaps, "libx265"},
		{"cpu h264", "cpu", "h264", cpuCaps, "libx264"},
		{"cpu av1 prefers svt-av1", "cpu", "av1", cpuCaps, "libsvtav1"},
		{"cpu av1 falls to aom", "cpu", "av1", EncoderCaps{HasLibx265: true, HasLibaomAV1: true, RecommendedEncoder: "libx265"}, "libaom-av1"},
		{"nvenc hevc", "nvenc", "hevc", nvencCaps, "hevc_nvenc"},
		{"nvenc h264", "nvenc", "h264", nvencCaps, "h264_nvenc"},
		{"nvenc av1 requires verified hw", "nvenc", "av1", nvencCaps, "av1_nvenc"},
		{"qsv hevc", "qsv", "hevc", qsvCaps, "hevc_qsv"},
		{"qsv h264", "qsv", "h264", qsvCaps, "h264_qsv"},

		// Fallbacks: the requested engine is unavailable, so the agent's own
		// tiered recommendation must win — a preset must never fail a job
		// because the machine lacks the GPU.
		{"nvenc unavailable falls back to recommendation", "nvenc", "hevc", cpuCaps, "libx265"},
		{"qsv unavailable falls back to recommendation", "qsv", "hevc", nvencCaps, "hevc_nvenc"},
		{"nvenc av1 unverified falls back to cpu av1", "nvenc", "av1", EncoderCaps{HasNvencHEVC: true, HasNvencAV1: true, HasLibsvtAV1: true, RecommendedEncoder: "hevc_nvenc"}, "libsvtav1"},
		{"qsv av1 unverified falls back to cpu av1", "qsv", "av1", EncoderCaps{HasIntel: true, HasQSVHEVC: true, HasLibsvtAV1: true, RecommendedEncoder: "hevc_qsv"}, "libsvtav1"},
		{"nvenc av1 unverified falls back to recommendation when no cpu av1", "nvenc", "av1", EncoderCaps{HasNvencHEVC: true, HasNvencAV1: true, RecommendedEncoder: "hevc_nvenc"}, "hevc_nvenc"},
		{"qsv av1 unverified falls back to recommendation when no cpu av1", "qsv", "av1", qsvCaps, "hevc_qsv"},
		{"cpu hevc without libx265 falls back", "cpu", "hevc", EncoderCaps{HasLibx264: true, RecommendedEncoder: "libx264"}, "libx264"},
		{"unknown engine falls back", "bogus", "hevc", cpuCaps, "libx265"},
		{"unverified nvenc without hardware falls back to recommendation", "nvenc", "hevc", EncoderCaps{HasNvencHEVC: true, HasLibx265: true, RecommendedEncoder: "libx265"}, "libx265"},
		{"unverified nvenc falls back to qsv when qsv verified", "nvenc", "hevc", EncoderCaps{HasNvencHEVC: true, HasQSVHEVC: true, HasQSVHEVCTuningVerified: true, RecommendedEncoder: "hevc_qsv"}, "hevc_qsv"},
		{"unverified qsv without canary falls back to cpu", "qsv", "hevc", EncoderCaps{HasIntel: true, HasQSVHEVC: true, HasLibx265: true, RecommendedEncoder: "libx265"}, "libx265"},

		// Recommendation derived on the fly when caps were never probed.
		{"empty recommendation derives one", "nvenc", "hevc", EncoderCaps{HasIntel: true, HasQSVHEVC: true, HasQSVHEVCVerified: true}, "hevc_qsv"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveEncoder(tt.requested, tt.targetCodec, tt.caps); got != tt.want {
				t.Errorf("ResolveEncoder(%q, %q, caps) = %q, want %q", tt.requested, tt.targetCodec, got, tt.want)
			}
		})
	}
}
