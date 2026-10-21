package ffmpeg

import (
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
)

func TestIsHardwareEncoder(t *testing.T) {
	hw := []string{"hevc_nvenc", "h264_nvenc", "av1_nvenc", "hevc_qsv", "h264_qsv", "av1_qsv", "hevc_amf", "h264_amf", "av1_amf", "hevc_videotoolbox", "h264_videotoolbox"}
	for _, e := range hw {
		if !IsHardwareEncoder(e) {
			t.Errorf("IsHardwareEncoder(%q) = false, want true", e)
		}
	}
	sw := []string{"libx264", "libx265", "libaom-av1", "libsvtav1", "hevc", "h264", "av1", "vp9", "mpeg4", "copy"}
	for _, e := range sw {
		if IsHardwareEncoder(e) {
			t.Errorf("IsHardwareEncoder(%q) = true, want false", e)
		}
	}
}

func TestUseChunkedPipeline(t *testing.T) {
	nvencCaps := &EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: true, HasLibx265: true, RecommendedEncoder: "hevc_nvenc"}
	swCaps := &EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}

	cases := []struct {
		name        string
		spec        jobspec.JobSpec
		targetCodec string
		caps        *EncoderCaps
		want        bool
	}{
		{
			name: "software engine resolves software → chunked",
			spec: jobspec.JobSpec{EncoderEngine: "cpu"},
			caps: nvencCaps,
			want: true,
		},
		{
			name: "no engine request with family codec → chunked",
			spec: jobspec.JobSpec{},
			caps: nvencCaps,
			want: true,
		},
		{
			name: "nvenc engine resolves hevc_nvenc → single pass",
			spec: jobspec.JobSpec{EncoderEngine: "nvenc"},
			caps: nvencCaps,
			want: false,
		},
		{
			name: "qsv engine resolves hevc_qsv → single pass",
			spec: jobspec.JobSpec{EncoderEngine: "qsv"},
			caps: &EncoderCaps{HasQSVHEVC: true, HasQSVHEVCTuningVerified: true, RecommendedEncoder: "hevc_qsv"},
			want: false,
		},
		{
			name:        "concrete hardware encoder as target codec → single pass",
			spec:        jobspec.JobSpec{},
			targetCodec: "hevc_nvenc",
			caps:        nvencCaps,
			want:        false,
		},
		{
			name: "hardware request without hardware falls back to software → chunked",
			spec: jobspec.JobSpec{EncoderEngine: "nvenc"},
			caps: swCaps,
			want: true,
		},
		{
			name: "override forces chunked on a hardware engine",
			spec: jobspec.JobSpec{EncoderEngine: "nvenc", Chunking: jobspec.ChunkingChunked},
			caps: nvencCaps,
			want: true,
		},
		{
			name: "override forces single pass on a software engine",
			spec: jobspec.JobSpec{EncoderEngine: "cpu", Chunking: jobspec.ChunkingSinglePass},
			caps: swCaps,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codec := tc.targetCodec
			if codec == "" {
				codec = "hevc"
			}
			if got := UseChunkedPipeline(tc.spec, codec, tc.caps); got != tc.want {
				t.Errorf("UseChunkedPipeline(%+v, %s) = %v, want %v", tc.spec, codec, got, tc.want)
			}
		})
	}

	// Nil caps must behave like zero caps (software fallback), not panic.
	if !UseChunkedPipeline(jobspec.JobSpec{}, "hevc", nil) {
		t.Errorf("UseChunkedPipeline with nil caps = false, want true (software fallback)")
	}
}
