package protocol

// IsValidCodec reports whether c is a supported video transcode codec or encoder name.
func IsValidCodec(c string) bool {
	switch c {
	case "hevc", "h264", "h265", "av1", "vp9", "vp8", "mpeg4", "mpeg2video",
		"libx264", "libx265", "libaom-av1", "libsvtav1",
		"hevc_nvenc", "h264_nvenc", "hevc_qsv", "h264_qsv",
		"hevc_amf", "h264_amf", "hevc_videotoolbox", "h264_videotoolbox",
		"av1_nvenc", "av1_qsv", "av1_amf":
		return true
	default:
		return false
	}
}

// IsValidPreset reports whether p is a recognized speed/quality preset identifier.
func IsValidPreset(p string) bool {
	if p == "" {
		return true
	}
	switch p {
	case "ultrafast", "superfast", "veryfast", "faster", "fast", "medium",
		"slow", "slower", "veryslow",
		"p1", "p2", "p3", "p4", "p5", "p6", "p7",
		"default", "hq", "llhq", "llhp", "balanced", "speed", "quality",
		"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13":
		return true
	default:
		return false
	}
}

// IsValidAudioCodec reports whether a is a supported audio transcode or passthrough codec.
func IsValidAudioCodec(a string) bool {
	if a == "" {
		return true
	}
	switch a {
	case "copy", "aac", "opus", "libopus", "mp3", "libmp3lame",
		"flac", "alac", "ac3", "eac3", "truehd", "dts",
		"pcm_s16le", "pcm_s24le", "vorbis", "libvorbis":
		return true
	default:
		return false
	}
}
