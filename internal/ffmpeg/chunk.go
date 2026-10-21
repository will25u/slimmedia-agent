package ffmpeg

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// probeStreamCodecTypes lists the codec_type of every stream in the file via
// ffprobe ("video", "audio", "subtitle", "data", "attachment", ...). A failed
// probe returns an error — chunk verification must never silently degrade,
// since a chunk that smuggles non-video streams into concatenation reintroduces
// the seam artifacts the video-only encode exists to eliminate.
func probeStreamCodecTypes(ffprobePath, filePath string) ([]string, error) {
	if err := CheckLocalMediaPath(filePath); err != nil {
		return nil, fmt.Errorf("invalid probe path: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := execCommandContext(ctx, ffprobePath,
		"-v", "error",
		"-show_entries", "stream=codec_type",
		"-of", "json",
		filePath,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("stream probe failed: %w", err)
	}
	var output struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &output); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe stream json: %w", err)
	}
	types := make([]string, 0, len(output.Streams))
	for _, s := range output.Streams {
		types = append(types, s.CodecType)
	}
	return types, nil
}

// isPureVideoStreamList is the video-only chunk invariant: exactly one stream
// and it is video.
func isPureVideoStreamList(types []string) bool {
	return len(types) == 1 && types[0] == "video"
}

// verifyChunkVideoOnly enforces that an encoded chunk carries exactly one
// stream and that it is video — the ffprobe acceptance gate for video-only
// chunk encoding (-an -sn -dn). Chunks violating the invariant (e.g. produced
// by an older agent that copied audio into segments) are rejected so the
// caller can re-encode rather than let seam overhang reach concatenation. A
// probe failure is returned as an error rather than a verdict: the caller
// must treat "could not verify" differently from "verified non-compliant".
func verifyChunkVideoOnly(ffprobePath, chunkPath string) error {
	types, err := probeStreamCodecTypes(ffprobePath, chunkPath)
	if err != nil {
		return err
	}
	if !isPureVideoStreamList(types) {
		return fmt.Errorf("chunk %s has streams %v, want exactly one video stream", chunkPath, types)
	}
	return nil
}

// VerifyChunkCandidate verifies a candidate encoded chunk for reuse or recovery:
// checks that it is pure video, matches expected duration within 2s tolerance,
// and passes an ffmpeg -sseof -1 tail decode.
func VerifyChunkCandidate(ctx context.Context, ffmpegPath, ffprobePath, chunkFile string, expectedDur float64) error {
	if err := verifyChunkVideoOnly(ffprobePath, chunkFile); err != nil {
		return err
	}
	return verifyChunkIntegrity(ctx, ffmpegPath, ffprobePath, chunkFile, expectedDur)
}

func verifyChunkIntegrity(ctx context.Context, ffmpegPath, ffprobePath, chunkFile string, expectedDur float64) error {
	info, err := ProbeMedia(ffprobePath, chunkFile)
	if err != nil {
		return fmt.Errorf("media probe failed: %w", err)
	}

	if expectedDur > 0 && info.Duration > 0 && math.Abs(info.Duration-expectedDur) > 2.0 {
		return fmt.Errorf("duration mismatch: got %.2fs, expected %.2fs", info.Duration, expectedDur)
	}

	tailCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := execCommandContext(tailCtx, ffmpegPath, "-v", "error", "-nostdin", "-sseof", "-1", "-i", chunkFile, "-f", "null", "-")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tail decode failed: %w", err)
	}

	return nil
}


