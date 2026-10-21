package ffmpeg

import (
	"github.com/will25u/slimmedia-agent/internal/jobspec"
)

// IsHardwareEncoder reports whether a concrete FFmpeg encoder name is a
// hardware encoder (NVENC, QSV, AMF, VideoToolbox). Software encoders and
// bare codec-family names ("hevc", "h264", "av1") report false.
func IsHardwareEncoder(encoder string) bool {
	return encoderVendor(encoder) != "software"
}

// UseChunkedPipeline decides whether a job runs the split-first chunked
// pipeline or a single full-stream encode. Chunking pays off only for
// software encoders: it unlocks chunk-granular pause and parallel-safe crash
// recovery. Hardware encoders run one full-stream pass — the split phase
// costs about as much as the encode itself (a second, I/O-bound read of the
// source) while buying nothing, since a single pass has no chunk boundary at
// which pause could take effect anyway.
//
// The decision is made on the resolved encoder, so a hardware-engine request
// that falls back to a software encoder (no GPU, unverified AV1 canary, or a
// stripped capability set) automatically returns to the chunked path. A
// spec-level override forces either mode regardless of the encoder.
func UseChunkedPipeline(spec jobspec.JobSpec, targetCodec string, caps *EncoderCaps) bool {
	if chunked, ok := spec.ChunkingOverride(); ok {
		return chunked
	}
	var c EncoderCaps
	if caps != nil {
		c = *caps
	}
	resolved := ResolveEncoder(spec.EncoderEngine, targetCodec, c)
	return !IsHardwareEncoder(resolved)
}
