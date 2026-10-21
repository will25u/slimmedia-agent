package protocol_test

import (
	"testing"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func TestIsValidCodec(t *testing.T) {
	valid := []string{
		"hevc", "h264", "h265", "av1", "vp9", "vp8", "mpeg4", "mpeg2video",
		"libx264", "libx265", "libaom-av1", "libsvtav1",
		"hevc_nvenc", "h264_nvenc", "hevc_qsv", "h264_qsv",
		"hevc_amf", "h264_amf", "hevc_videotoolbox", "h264_videotoolbox",
		"av1_nvenc", "av1_qsv", "av1_amf",
	}
	for _, c := range valid {
		if !protocol.IsValidCodec(c) {
			t.Errorf("expected codec %q to be valid", c)
		}
	}

	invalid := []string{"", "invalid", "copy", "flac", "aac", "h266"}
	for _, c := range invalid {
		if protocol.IsValidCodec(c) {
			t.Errorf("expected codec %q to be invalid", c)
		}
	}
}

func TestIsValidPreset(t *testing.T) {
	valid := []string{
		"", "ultrafast", "superfast", "veryfast", "faster", "fast", "medium",
		"slow", "slower", "veryslow",
		"p1", "p2", "p3", "p4", "p5", "p6", "p7",
		"default", "hq", "llhq", "llhp", "balanced", "speed", "quality",
		"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13",
	}
	for _, p := range valid {
		if !protocol.IsValidPreset(p) {
			t.Errorf("expected preset %q to be valid", p)
		}
	}

	invalid := []string{"invalid", "turbo", "max", "14"}
	for _, p := range invalid {
		if protocol.IsValidPreset(p) {
			t.Errorf("expected preset %q to be invalid", p)
		}
	}
}

func TestIsValidAudioCodec(t *testing.T) {
	valid := []string{
		"", "copy", "aac", "opus", "libopus", "mp3", "libmp3lame",
		"flac", "alac", "ac3", "eac3", "truehd", "dts",
		"pcm_s16le", "pcm_s24le", "vorbis", "libvorbis",
	}
	for _, a := range valid {
		if !protocol.IsValidAudioCodec(a) {
			t.Errorf("expected audio codec %q to be valid", a)
		}
	}

	invalid := []string{"invalid", "hevc", "h264", "vorbis2"}
	for _, a := range invalid {
		if protocol.IsValidAudioCodec(a) {
			t.Errorf("expected audio codec %q to be invalid", a)
		}
	}
}
