package ffmpeg

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyResolution(t *testing.T) {
	tests := []struct {
		name   string
		width  int
		height int
		want   string
	}{
		{"4K UHD standard", 3840, 2160, "4K"},
		{"4K DCI scope", 4096, 1716, "4K"},
		{"4K vertical", 2160, 3840, "4K"},
		{"1080p standard", 1920, 1080, "1080p"},
		{"1080p scope", 1920, 800, "1080p"},
		{"720p standard", 1280, 720, "720p"},
		{"720p crop", 1280, 534, "720p"},
		{"SD 576p PAL", 720, 576, "SD"},
		{"SD 480p NTSC", 720, 480, "SD"},
		{"Zero dimensions", 0, 0, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyResolution(tt.width, tt.height)
			if got != tt.want {
				t.Errorf("ClassifyResolution(%d, %d) = %q, want %q", tt.width, tt.height, got, tt.want)
			}
		})
	}
}

func TestProbeMediaResolutionUnmarshaling(t *testing.T) {
	sampleJSON := `{
		"format": {
			"duration": "120.5",
			"size": "104857600",
			"bit_rate": "8000000"
		},
		"streams": [
			{
				"codec_name": "hevc",
				"codec_type": "video",
				"width": 3840,
				"height": 2160,
				"color_transfer": "smpte2084"
			}
		]
	}`

	var out ffprobeOutput
	if err := json.Unmarshal([]byte(sampleJSON), &out); err != nil {
		t.Fatalf("failed to unmarshal test JSON: %v", err)
	}

	if len(out.Streams) == 0 {
		t.Fatal("expected at least 1 stream")
	}

	s := out.Streams[0]
	if s.Width != 3840 || s.Height != 2160 {
		t.Errorf("expected width=3840, height=2160; got width=%d, height=%d", s.Width, s.Height)
	}
}

// TestProbeMediaVideoStreamDuration: the stream-level duration must be
// exposed separately from the container format duration so duration
// verification can compare like-for-like values (concatenated Matroska
// inflates container duration at chunk junctions). When ffprobe reports no
// stream-level duration, the field stays zero and callers fall back to the
// container comparison.
func TestProbeMediaVideoStreamDuration(t *testing.T) {
	tmp := t.TempDir()
	media := filepath.Join(tmp, "in.mkv")
	if err := os.WriteFile(media, []byte("fake"), 0644); err != nil {
		t.Fatalf("failed to stage media file: %v", err)
	}

	withStreamDur := fakeTool(t, tmp, "ffprobe-stream", `#!/bin/sh
cat <<'PROBE_EOF'
{"format":{"duration":"5751.960000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"hevc","duration":"5748.790000","width":3840,"height":2160}]}
PROBE_EOF
`)

	info, err := ProbeMedia(withStreamDur, media)
	if err != nil {
		t.Fatalf("ProbeMedia failed: %v", err)
	}
	if info.VideoStreamDuration != 5748.79 {
		t.Fatalf("VideoStreamDuration = %.4f, want 5748.79", info.VideoStreamDuration)
	}
	if info.Duration != 5751.96 {
		t.Fatalf("container Duration = %.4f, want 5751.96 (must remain the format duration)", info.Duration)
	}

	noStreamDur := fakeTool(t, tmp, "ffprobe-nostream", `#!/bin/sh
cat <<'PROBE_EOF'
{"format":{"duration":"6000.000000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"hevc","width":1920,"height":1080}]}
PROBE_EOF
`)

	info2, err := ProbeMedia(noStreamDur, media)
	if err != nil {
		t.Fatalf("ProbeMedia failed: %v", err)
	}
	if info2.VideoStreamDuration != 0 {
		t.Fatalf("VideoStreamDuration = %.4f, want 0 when ffprobe reports no stream duration", info2.VideoStreamDuration)
	}
}

// TestProbeMediaStreamStartTimes: the video and best audio streams' start_time
// values must be exposed (with presence flags — a genuine 0.0 video start is
// common) so a/v alignment verification can compare input vs output offsets.
func TestProbeMediaStreamStartTimes(t *testing.T) {
	tmp := t.TempDir()
	media := filepath.Join(tmp, "in.mkv")
	if err := os.WriteFile(media, []byte("fake"), 0644); err != nil {
		t.Fatalf("failed to stage media file: %v", err)
	}

	withStarts := fakeTool(t, tmp, "ffprobe-starts", `#!/bin/sh
cat <<'PROBE_EOF'
{"format":{"duration":"120.000000","size":"1048576","bit_rate":"1000"},"streams":[
 {"codec_type":"video","codec_name":"h264","start_time":"0.000000","width":1920,"height":1080},
 {"codec_type":"audio","codec_name":"aac","start_time":"0.600000"},
 {"codec_type":"audio","codec_name":"ac3","start_time":"0.600000"}
]}
PROBE_EOF
`)

	info, err := ProbeMedia(withStarts, media)
	if err != nil {
		t.Fatalf("ProbeMedia failed: %v", err)
	}
	if !info.VideoStartTimeSet || info.VideoStartTime != 0.0 {
		t.Fatalf("VideoStartTime = %v (set=%v), want 0.0 with set=true", info.VideoStartTime, info.VideoStartTimeSet)
	}
	if !info.AudioStartTimeSet || math.Abs(info.AudioStartTime-0.6) > 1e-9 {
		t.Fatalf("AudioStartTime = %v (set=%v), want 0.6 with set=true", info.AudioStartTime, info.AudioStartTimeSet)
	}
	if offset, ok := info.AVOffset(); !ok || math.Abs(offset-0.6) > 1e-9 {
		t.Fatalf("AVOffset = %v (ok=%v), want 0.6 with ok=true", offset, ok)
	}

	noStarts := fakeTool(t, tmp, "ffprobe-nostarts", `#!/bin/sh
cat <<'PROBE_EOF'
{"format":{"duration":"120.000000","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080}]}
PROBE_EOF
`)

	info2, err := ProbeMedia(noStarts, media)
	if err != nil {
		t.Fatalf("ProbeMedia failed: %v", err)
	}
	if info2.VideoStartTimeSet || info2.AudioStartTimeSet {
		t.Fatalf("start times must be unset when ffprobe reports none: video=%v audio=%v", info2.VideoStartTimeSet, info2.AudioStartTimeSet)
	}
	if _, ok := info2.AVOffset(); ok {
		t.Fatal("AVOffset ok must be false when start times are missing")
	}
}

func TestProbeMediaExactFramesKnown(t *testing.T) {
	tmp := t.TempDir()
	media := filepath.Join(tmp, "in.mkv")
	if err := os.WriteFile(media, []byte("fake"), 0644); err != nil {
		t.Fatalf("failed to stage media file: %v", err)
	}

	withNbFrames := fakeTool(t, tmp, "ffprobe-nbframes", `#!/bin/sh
cat <<'PROBE_EOF'
{"format":{"duration":"10.0","size":"1048576","bit_rate":"1000"},"streams":[
 {"codec_type":"video","codec_name":"h264","nb_frames":"240","r_frame_rate":"24/1","width":1920,"height":1080}
]}
PROBE_EOF
`)

	info, err := ProbeMedia(withNbFrames, media)
	if err != nil {
		t.Fatalf("ProbeMedia failed: %v", err)
	}
	if info.TotalFrames != 240 {
		t.Fatalf("TotalFrames = %d, want 240", info.TotalFrames)
	}
	if !info.ExactFramesKnown {
		t.Fatalf("ExactFramesKnown = %v, want true when nb_frames is in stream header", info.ExactFramesKnown)
	}

	estimatedFrames := fakeTool(t, tmp, "ffprobe-estimated", `#!/bin/sh
cat <<'PROBE_EOF'
{"format":{"duration":"10.0","size":"1048576","bit_rate":"1000"},"streams":[
 {"codec_type":"video","codec_name":"h264","r_frame_rate":"24/1","width":1920,"height":1080}
]}
PROBE_EOF
`)

	info2, err := ProbeMedia(estimatedFrames, media)
	if err != nil {
		t.Fatalf("ProbeMedia failed: %v", err)
	}
	if info2.TotalFrames != 240 {
		t.Fatalf("TotalFrames = %d, want 240 (derived from duration * fps)", info2.TotalFrames)
	}
	if info2.ExactFramesKnown {
		t.Fatalf("ExactFramesKnown = %v, want false when nb_frames is missing and derived", info2.ExactFramesKnown)
	}
}

