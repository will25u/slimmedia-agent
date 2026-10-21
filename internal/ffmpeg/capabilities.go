package ffmpeg

import (
	"bytes"
	"context"
	"encoding/base64"
	"log"
	"strings"
	"time"
)

// EncoderCaps describes what encoders/decoders this FFmpeg binary can use.
type EncoderCaps struct {
	// Software
	HasLibx264   bool
	HasLibx265   bool
	HasLibaomAV1 bool
	HasLibsvtAV1 bool
	// NVIDIA NVENC
	HasNvencH264 bool
	HasNvencHEVC bool
	HasNvencAV1  bool
	// Intel Quick Sync
	HasQSVH264 bool
	HasQSVHEVC bool
	HasQSVAV1  bool
	// AMD AMF
	HasAMFH264 bool
	HasAMFHEVC bool
	HasAMFAV1  bool
	// Apple VideoToolbox
	HasVTH264 bool
	HasVTHEVC bool

	// AV1 hardware encode, proven by a canary encode on this machine.
	// Presence in `ffmpeg -encoders` alone is not sufficient: a build can
	// list av1_qsv while the GPU (e.g. Intel UHD 770) has no AV1 encode
	// hardware, drivers, or device access.
	HasNvencAV1Verified bool
	HasQSVAV1Verified   bool
	HasAMFAV1Verified   bool

	// QSV hardware encode, proven by a live canary encode on this machine.
	// PCI device presence alone is not sufficient (headless boxes, missing firmware,
	// or containers without device passthrough).
	HasQSVHEVCVerified bool
	HasQSVH264Verified bool

	// Advanced tuning flags proven by a canary encode on this machine: the
	// driver accepts the encoder's full tuning flag set. Pre-Gen9 QSV
	// rejects -extbrc and pre-Turing NVENC rejects -b_ref_mode, so a false
	// here means buildFfmpegArgs skips the tuning block (quality args still
	// apply) instead of failing the job.
	HasNvencTuningVerified    bool
	HasNvencAV1TuningVerified bool
	HasQSVHEVCTuningVerified  bool
	HasQSVH264TuningVerified  bool
	HasQSVAV1TuningVerified   bool
	HasAMFTuningVerified      bool

	// Archivist-grade QSV package, proven by canary encodes on this machine.
	// Pre-Gen9 QSV rejects -extbrc and some drivers reject -adaptive_i /
	// -adaptive_b, so each flag group is verified independently and stripped
	// individually when its canary fails. Lookahead is deliberately NOT part
	// of this package: the driver silently ignores it under constant-quality
	// (ICQ) rate control, so emitting it would be a false promise.
	HasQSVExtBRCVerified     bool
	HasQSVAdaptiveIBVerified bool

	// Hardware acceleration backends (from ffmpeg -hwaccels)
	HasHWAccelCUDA   bool
	HasHWAccelVAAPI  bool
	HasHWAccelD3D11VA bool
	HasHWAccelQSV    bool

	// Hardware decode verification via canaries
	HasHWDecodeHEVC10Verified bool
	HasHWDecodeAV1Verified    bool

	// Hardware HDR tone-mapping filter verification via canaries
	HasTonemapOpenCLVerified bool
	HasTonemapCUDAVerified   bool
	HasTonemapVAAPIVerified  bool
	HasHWTonemapVerified     bool

	// Hardware presence flags
	HasNvidia bool
	HasIntel  bool
	HasAMD    bool

	// Derived recommendation
	RecommendedEncoder string
	RecommendedPreset  string
	RecommendedCRF     int
	EstimatedFPS       float64
}

// ProbeEncoders runs ffmpeg -encoders and -decoders to determine available
// encode/decode capabilities, then derives a recommended configuration.
func ProbeEncoders(ffmpegPath string) EncoderCaps {
	var caps EncoderCaps

	// --- Encoders ---
	out, err := execCommand(ffmpegPath, "-encoders", "-v", "quiet").Output()
	if err == nil {
		caps = parseEncoderCaps(string(out))
	}

	// --- HWaccel backends ---
	if outHw, err := execCommand(ffmpegPath, "-hwaccels", "-v", "quiet").Output(); err == nil {
		caps.HasHWAccelCUDA, caps.HasHWAccelVAAPI, caps.HasHWAccelD3D11VA, caps.HasHWAccelQSV = parseHWAccels(string(outHw))
	}

	// --- Verify base QSV encoders via live canary encode ---
	verifyQSVEncoders(ffmpegPath, &caps)

	// --- Verify AV1 hardware encoders against the real driver stack ---
	verifyAV1Encoders(ffmpegPath, &caps)

	// --- Verify advanced tuning flags for hw encoders ---
	verifyTuningFlags(ffmpegPath, &caps)

	// --- Verify the Archivist-grade QSV package per driver ---
	verifyArchivistQSVFlags(ffmpegPath, &caps)

	// --- Verify hardware decoders via canaries ---
	verifyHWDecoders(ffmpegPath, &caps)

	// --- Verify hardware tone-mapping filters via canaries ---
	verifyHWTonemapFilters(ffmpegPath, &caps)

	// --- Derive recommendation ---
	caps.RecommendedEncoder, caps.RecommendedPreset, caps.RecommendedCRF, caps.EstimatedFPS = deriveRecommendation(caps)
	return caps
}

// av1CanaryTimeout bounds a single canary encode. Machines without the right
// silicon or driver fail encoder init in milliseconds; the timeout only
// guards against a hung driver.
const av1CanaryTimeout = 15 * time.Second

// av1CanaryArgs holds codec-specific quality arguments for each AV1 hardware
// encoder's throwaway encode.
var av1CanaryArgs = map[string][]string{
	"av1_nvenc": {"-rc", "constqp", "-qp", "30"},
	"av1_qsv":   {"-global_quality", "30"},
	"av1_amf":   {"-quality", "speed", "-rc", "cqp", "-qp_i", "30", "-qp_p", "30"},
}

// verifyAV1Encoders functionally tests each AV1 hardware encoder the binary
// claims to have. This is the ground truth for "this chip can encode AV1":
// it exercises the GPU, its driver, and device access (e.g. /dev/dri),
// not just the FFmpeg build configuration.
func verifyAV1Encoders(ffmpegPath string, caps *EncoderCaps) {
	if caps.HasNvencAV1 {
		caps.HasNvencAV1Verified = canaryEncode(ffmpegPath, "av1_nvenc", av1CanaryArgs["av1_nvenc"]...)
	}
	if caps.HasQSVAV1 {
		caps.HasQSVAV1Verified = canaryEncode(ffmpegPath, "av1_qsv", av1CanaryArgs["av1_qsv"]...)
	}
	if caps.HasAMFAV1 {
		caps.HasAMFAV1Verified = canaryEncode(ffmpegPath, "av1_amf", av1CanaryArgs["av1_amf"]...)
	}
}

// verifyQSVEncoders functionally tests base QSV HEVC and H264 hardware encoders
// via live canary encodes. This prevents false capability detection on headless machines,
// containers without GPU passthrough, or systems where the iGPU is disabled in firmware.
func verifyQSVEncoders(ffmpegPath string, caps *EncoderCaps) {
	if caps.HasQSVHEVC {
		caps.HasQSVHEVCVerified = canaryEncode(ffmpegPath, "hevc_qsv")
	}
	if caps.HasQSVH264 {
		caps.HasQSVH264Verified = canaryEncode(ffmpegPath, "h264_qsv")
	}
}

// canaryEncode runs a 1-frame testsrc2 encode with the given encoder
// and optional extra args, discarding the output. It returns true
// only if the encode succeeds end-to-end.
func canaryEncode(ffmpegPath, encoder string, extraArgs ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), av1CanaryTimeout)
	defer cancel()

	args := []string{
		"-hide_banner", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=128x72:rate=1:duration=1",
		"-c:v", encoder,
	}
	args = append(args, extraArgs...)
	args = append(args, "-f", "null", "-")

	return execCommandContext(ctx, ffmpegPath, args...).Run() == nil
}

// verifyTuningFlags probes whether each hardware HEVC encoder's
// advanced tuning flags (hwTuningFlagSets) are accepted by the driver. A
// failed canary means pre-Gen9 QSV (rejects -extbrc) or pre-Turing NVENC
// (rejects -b_ref_mode): the tuning flags are skipped at encode
// time rather than failing the job.
func verifyTuningFlags(ffmpegPath string, caps *EncoderCaps) {
	if caps.HasNvencHEVC {
		caps.HasNvencTuningVerified = canaryEncode(ffmpegPath, "hevc_nvenc", hwTuningFlagSets["hevc_nvenc"]...)
	}
	if caps.HasNvencAV1 || caps.HasNvencAV1Verified {
		caps.HasNvencAV1TuningVerified = canaryEncode(ffmpegPath, "av1_nvenc", hwTuningFlagSets["av1_nvenc"]...)
	}
	if caps.HasQSVHEVC {
		caps.HasQSVHEVCTuningVerified = canaryEncode(ffmpegPath, "hevc_qsv", hwTuningFlagSets["hevc_qsv"]...)
	}
	if caps.HasQSVH264 {
		caps.HasQSVH264TuningVerified = canaryEncode(ffmpegPath, "h264_qsv", hwTuningFlagSets["h264_qsv"]...)
	}
	if caps.HasQSVAV1 || caps.HasQSVAV1Verified {
		caps.HasQSVAV1TuningVerified = canaryEncode(ffmpegPath, "av1_qsv", hwTuningFlagSets["av1_qsv"]...)
	}
	if caps.HasAMFHEVC {
		caps.HasAMFTuningVerified = canaryEncode(ffmpegPath, "hevc_amf", hwTuningFlagSets["hevc_amf"]...)
	}
}

// archivistQSVCanaryArgs builds the throwaway-encode argv for the QSV
// Archivist canaries from the SAME code-owned package the builder emits
// (archivistPackages), so a driver that passes the canary always accepts
// the real encode. extra carries the tuning flag group under test. The
// second return is false when the package has no QSV entry — verification
// must not run (and must not verify) an undefined package.
func archivistQSVCanaryArgs(extra ...string) ([]string, bool) {
	pkg, ok := archivistPackages["hevc_qsv"]
	if !ok {
		return nil, false
	}
	args := append([]string{}, pkg.quality...)
	args = append(args, "-preset", pkg.speed)
	return append(args, extra...), true
}

// verifyArchivistQSVFlags proves, per driver, which flags of the
// Archivist-grade QSV package this machine accepts. The full package
// (extended bitrate control + adaptive I/B frames) is tried first; on
// failure each flag group is retried alone, so a driver that rejects one
// group still keeps the other. Each failed group is stripped at encode
// time rather than failing the job.
func verifyArchivistQSVFlags(ffmpegPath string, caps *EncoderCaps) {
	if !caps.HasQSVHEVC {
		return
	}
	full, ok := archivistQSVCanaryArgs("-extbrc", "1", "-adaptive_i", "1", "-adaptive_b", "1")
	if !ok {
		return
	}
	if canaryEncode(ffmpegPath, "hevc_qsv", full...) {
		caps.HasQSVExtBRCVerified = true
		caps.HasQSVAdaptiveIBVerified = true
		return
	}
	extbrc, _ := archivistQSVCanaryArgs("-extbrc", "1")
	if canaryEncode(ffmpegPath, "hevc_qsv", extbrc...) {
		caps.HasQSVExtBRCVerified = true
	}
	adaptive, _ := archivistQSVCanaryArgs("-adaptive_i", "1", "-adaptive_b", "1")
	if canaryEncode(ffmpegPath, "hevc_qsv", adaptive...) {
		caps.HasQSVAdaptiveIBVerified = true
	}
}

const hevc10bitCanarySampleB64 = "AAAAAUABDAH//wQIAAADAJ2oAAADAAAeugJAAAAAAUIBAQQIAAADAJ2oAAADAAAeoEIITZbq5MrwFoCAAAADAIAAAAMAhAAAAAFEAcFzwIkAAAFOAQX//////////98sot4JtRdH27tVpP5/wvxOeDI2NSAoYnVpbGQgMTk5KSAtIDMuNSsxLWYwYzEwMjJiNjpbTGludXhdW0dDQyAxMS4yLjBdWzY0IGJpdF0gMTBiaXQgLSBILjI2NS9IRVZDIGNvZGVjIC0gQ29weXJpZ2h0IDIwMTMtMjAxOCAoYykgTXVsdGljb3Jld2FyZSwgSW5jIC0gaHR0cDovL3gyNjUub3JnIC0gb3B0aW9uczogY3B1aWQ9MTA0OTU4MyBmcmFtZS10aHJlYWRzPTEgbm8td3BwIG5vLXBtb2RlIG5vLXBtZSBuby1wc25yIG5vLXNzaW0gbG9nLWxldmVsPS0xIGJpdGRlcHRoPTEwIGlucHV0LWNzcD0xIGZwcz0xLzEgaW5wdXQtcmVzPTMyeDMyIGludGVybGFjZT0wIHRvdGFsLWZyYW1lcz0wIGxldmVsLWlkYz0wIGhpZ2gtdGllcj0xIHVoZC1iZD0wIHJlZj0xIG5vLWFsbG93LW5vbi1jb25mb3JtYW5jZSByZXBlYXQtaGVhZGVycyBhbm5leGIgbm8tYXVkIG5vLWhyZCBpbmZvIGhhc2g9MCBuby10ZW1wb3JhbC1sYXllcnMgbm8tb3Blbi1nb3AgbWluLWtleWludD0xIGtleWludD0xIGdvcC1sb29rYWhlYWQ9MCBiZnJhbWVzPTAgYi1hZGFwdD0wIG5vLWItcHlyYW1pZCBiZnJhbWUtYmlhcz0wIHJjLWxvb2thaGVhZD0wIGxvb2thaGVhZC1zbGljZXM9MCBzY2VuZWN1dD0wIGhpc3Qtc2NlbmVjdXQ9MCByYWRsPTAgbm8tc3BsaWNlIG5vLWludHJhLXJlZnJlc2ggY3R1PTMyIG1pbi1jdS1zaXplPTggbm8tcmVjdCBuby1hbXAgbWF4LXR1LXNpemU9MzIgdHUtaW50ZXItZGVwdGg9MSB0dS1pbnRyYS1kZXB0aD0xIGxpbWl0LXR1PTAgcmRvcS1sZXZlbD0wIGR5bmFtaWMtcmQ9MC4wMCBuby1zc2ltLXJkIHNpZ25oaWRlIG5vLXRza2lwIG5yLWludHJhPTAgbnItaW50ZXI9MCBuby1jb25zdHJhaW5lZC1pbnRyYSBzdHJvbmctaW50cmEtc21vb3RoaW5nIG1heC1tZXJnZT0zIGxpbWl0LXJlZnM9MSBuby1saW1pdC1tb2RlcyBtZT0xIHN1Ym1lPTIgbWVyYW5nZT01NyB0ZW1wb3JhbC1tdnAgbm8tZnJhbWUtZHVwIG5vLWhtZSBuby13ZWlnaHRwIG5vLXdlaWdodGIgbm8tYW5hbHl6ZS1zcmMtcGljcyBkZWJsb2NrPTA6MCBzYW8gbm8tc2FvLW5vbi1kZWJsb2NrIHJkPTMgc2VsZWN0aXZlLXNhbz00IGVhcmx5LXNraXAgcnNraXAgbm8tZmFzdC1pbnRyYSBuby10c2tpcC1mYXN0IG5vLWN1LWxvc3NsZXNzIGItaW50cmEgbm8tc3BsaXRyZC1za2lwIHJkcGVuYWx0eT0wIHBzeS1yZD0yLjAwIHBzeS1yZG9xPTAuMDAgbm8tcmQtcmVmaW5lIG5vLWxvc3NsZXNzIGNicXBvZmZzPTAgY3JxcG9mZnM9MCByYz1jcmYgY3JmPTI4LjAgcWNvbXA9MC42MCBxcHN0ZXA9NCBzdGF0cy13cml0ZT0wIHN0YXRzLXJlYWQ9MCBpcHJhdGlvPTEuNDAgYXEtbW9kZT0yIGFxLXN0cmVuZ3RoPTEuMDAgbm8tY3V0cmVlIHpvbmUtY291bnQ9MCBuby1zdHJpY3QtY2JyIHFnLXNpemU9MzIgbm8tcmMtZ3JhaW4gcXBtYXg9NjkgcXBtaW49MCBuby1jb25zdC12YnYgc2FyPTEgb3ZlcnNjYW49MCB2aWRlb2Zvcm1hdD01IHJhbmdlPTAgY29sb3JwcmltPTIgdHJhbnNmZXI9MiBjb2xvcm1hdHJpeD0yIGNocm9tYWxvYz0wIGRpc3BsYXktd2luZG93PTAgY2xsPTAsMCBtaW4tbHVtYT0wIG1heC1sdW1hPTEwMjMgbG9nMi1tYXgtcG9jLWxzYj04IHZ1aS10aW1pbmctaW5mbyB2dWktaHJkLWluZm8gc2xpY2VzPTEgbm8tb3B0LXFwLXBwcyBuby1vcHQtcmVmLWxpc3QtbGVuZ3RoLXBwcyBuby1tdWx0aS1wYXNzLW9wdC1ycHMgc2NlbmVjdXQtYmlhcz0wLjA1IGhpc3QtdGhyZXNob2xkPTAuMDMgbm8tb3B0LWN1LWRlbHRhLXFwIG5vLWFxLW1vdGlvbiBuby1oZHIxMCBuby1oZHIxMC1vcHQgbm8tZGhkcjEwLW9wdCBuby1pZHItcmVjb3Zlcnktc2VpIGFuYWx5c2lzLXJldXNlLWxldmVsPTAgYW5hbHlzaXMtc2F2ZS1yZXVzZS1sZXZlbD0wIGFuYWx5c2lzLWxvYWQtcmV1c2UtbGV2ZWw9MCBzY2FsZS1mYWN0b3I9MCByZWZpbmUtaW50cmE9MCByZWZpbmUtaW50ZXI9MCByZWZpbmUtbXY9MSByZWZpbmUtY3R1LWRpc3RvcnRpb249MCBuby1saW1pdC1zYW8gY3R1LWluZm89MCBuby1sb3dwYXNzLWRjdCByZWZpbmUtYW5hbHlzaXMtdHlwZT0wIGNvcHktcGljPTEgbWF4LWF1c2l6ZS1mYWN0b3I9MS4wIG5vLWR5bmFtaWMtcmVmaW5lIG5vLXNpbmdsZS1zZWkgbm8taGV2Yy1hcSBuby1zdnQgbm8tZmllbGQgcXAtYWRhcHRhdGlvbi1yYW5nZT0xLjAwIHNjZW5lY3V0LWF3YXJlLXFwPTBjb25mb3JtYW5jZS13aW5kb3ctb2Zmc2V0cyByaWdodD0wIGJvdHRvbT0wIGRlY29kZXItbWF4LXJhdGU9MCBuby12YnYtbGl2ZS1tdWx0aS1wYXNzgAAAASgBr3irtJ4nQ19r2R5LUroyDh/WN8Q4KrpDFOjn7xyfj0rkj+UDXn2MCaueGldThRDJPJ7dmLNNCigVY0pJKVpFc6TF6VQ9v4zg3Lf4volB/+fVQv/hDRQXZDG7UOVR9u3liKhtjd66n4+3CuHZqCJMX9DwADB6t9cQ0VEUOHYC2S+R/i6a5aLVzPlE2JdEHw0pAKOK6RMbwmomlz0rVddpu7FkJ8zcY3goP9MnI2x7J0W+d8KgMiwCChHwLUkpqxxnXs5bu3JFqk7bAvQeXX/g6eFEIOqPoYjdWkIJEOf/IYhv4Ll7y8G/bk6Lim3h0XL0gZlQDv0u7A0gTZiMZc/qsl8gCMRp767y/nX5WQ4BrAnAhXwa612SprZbw29m0jmlI+3aYH74g3VNUeoJ+iSBMpo+3iZTHuFHSryKBPV7NUoZcbNCS0KVQ3t0zp0IcZmk3cLSOoz+C9RdclgngCO4z0vmG+0PJn+OISvb1ker4qgz5ClarIFmJgLq5E5YQPDC4xVh5faJ7g=="
const av1CanarySampleB64 = "REtJRgAAIABBVjAxQABAAAEAAAABAAAA//////////9DAgAAAAAAAAAAAAASAAoKAAAAAq//n/8wCDKyBBQAL+AMGMWJFqQI3agqp1J1OUNZVzCHRsQViYrTx8WKSckI16F0TImllzfhFNX1pQpe9kNsHnzSOUkb3+Nv7uS/QjAZXHDFwpHErgHhvPlF4np2D72EQ+74SMSY30Di5dn6h9ZrG1sDiydrmvmzjt/gcuw15nDfwyVuQxVqNZlS33M+MlRfLNX99RlT3D5ptbhE4B8oYuOp2qrr5UhjGTzUlKbQtj0PV1tt/rYp4udADHpnfY8WVsyHh+Cd4qGKCWek7QY2EYwPYvd2EMYgWj0llVAbsC5IdNQ5XW9/8dgFxFJBZz2Ug7xiCcjjrYxhzkmQ9ohB2jo/+OGg0RdqSwAk3wMf8KBFGlRh+5Ws9NbW79wRSlkcz+IRc+Zp7OCaKwxf7dvXOfrhP1mSClzpKDiV6i16LIkbJ3KpbrixT1XplXKji0fgjWZkd3LXtrrljESiBxLa/0+TZLBEm0/t+RgYn/6ys+EGfTSPUG4VMyVF22Szg7wjmr0Dk6SvhivxgKA/8vZbsOIVmYnFHlFtYlWHRz6iAUlW+AHn1c7BtI3BB21ZGvJdEIyPljuNnEBrBHcp18jSlQPWPvn+KnSaXCoIYcXt3mHcEyPwkmcBIqQycvWcdxoGCzAgH0D9L4wq/Qyrkp86cBEL1vlqX8sh/GKr6BOpHcsWoEdInfGvsvR4ulI4ZwW4ema/Wm/E+OwiQfcC7GZqIaP0AjMrL59bQUpMArbYYOzJCjFDtMT+uKaW7UA="

func parseHWAccels(output string) (cuda, vaapi, d3d11va, qsv bool) {
	lines := strings.Split(output, "\n")
	for _, l := range lines {
		switch strings.TrimSpace(l) {
		case "cuda":
			cuda = true
		case "vaapi":
			vaapi = true
		case "d3d11va":
			d3d11va = true
		case "qsv":
			qsv = true
		}
	}
	return
}

func canaryDecode(ffmpegPath, hwaccel string, sample []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), av1CanaryTimeout)
	defer cancel()

	cmd := execCommandContext(ctx, ffmpegPath,
		"-hide_banner", "-v", "error",
		"-hwaccel", hwaccel,
		"-i", "-",
		"-f", "null", "-",
	)
	cmd.Stdin = bytes.NewReader(sample)
	return cmd.Run() == nil
}

func verifyHWDecoders(ffmpegPath string, caps *EncoderCaps) {
	backends := make([]string, 0, 4)
	if caps.HasHWAccelCUDA {
		backends = append(backends, "cuda")
	}
	if caps.HasHWAccelVAAPI {
		backends = append(backends, "vaapi")
	}
	if caps.HasHWAccelD3D11VA {
		backends = append(backends, "d3d11va")
	}
	if caps.HasHWAccelQSV {
		backends = append(backends, "qsv")
	}
	if len(backends) == 0 {
		return
	}

	hevcSample, _ := base64.StdEncoding.DecodeString(hevc10bitCanarySampleB64)
	av1Sample, _ := base64.StdEncoding.DecodeString(av1CanarySampleB64)

	for _, backend := range backends {
		if !caps.HasHWDecodeHEVC10Verified && len(hevcSample) > 0 {
			if canaryDecode(ffmpegPath, backend, hevcSample) {
				caps.HasHWDecodeHEVC10Verified = true
			}
		}
		if !caps.HasHWDecodeAV1Verified && len(av1Sample) > 0 {
			if canaryDecode(ffmpegPath, backend, av1Sample) {
				caps.HasHWDecodeAV1Verified = true
			}
		}
	}
}

func canaryFilter(ffmpegPath string, args ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), av1CanaryTimeout)
	defer cancel()

	cmdArgs := append([]string{"-hide_banner", "-nostdin", "-v", "error"}, args...)
	return execCommandContext(ctx, ffmpegPath, cmdArgs...).Run() == nil
}

func verifyHWTonemapFilters(ffmpegPath string, caps *EncoderCaps) {
	caps.HasTonemapOpenCLVerified = canaryFilter(ffmpegPath,
		"-init_hw_device", "opencl=ocl",
		"-filter_hw_device", "ocl",
		"-f", "lavfi", "-i", "testsrc2=size=128x72:rate=1:duration=1",
		"-vf", "hwupload,tonemap_opencl=tonemap=mobius,hwdownload,format=yuv420p",
		"-f", "null", "-",
	)

	if caps.HasHWAccelCUDA {
		caps.HasTonemapCUDAVerified = canaryFilter(ffmpegPath,
			"-init_hw_device", "cuda=cu",
			"-filter_hw_device", "cu",
			"-f", "lavfi", "-i", "testsrc2=size=128x72:rate=1:duration=1",
			"-vf", "hwupload,scale_cuda=format=yuv420p",
			"-f", "null", "-",
		)
	}

	if caps.HasHWAccelVAAPI {
		caps.HasTonemapVAAPIVerified = canaryFilter(ffmpegPath,
			"-init_hw_device", "vaapi=va",
			"-filter_hw_device", "va",
			"-f", "lavfi", "-i", "testsrc2=size=128x72:rate=1:duration=1",
			"-vf", "format=nv12,hwupload,tonemap_vaapi=tonemap=mobius,hwdownload,format=nv12",
			"-f", "null", "-",
		)
	}

	caps.HasHWTonemapVerified = caps.HasTonemapOpenCLVerified || caps.HasTonemapCUDAVerified || caps.HasTonemapVAAPIVerified
}

// parseEncoderCaps maps an `ffmpeg -encoders` listing to EncoderCaps flags.
func parseEncoderCaps(encoders string) EncoderCaps {
	var caps EncoderCaps
	caps.HasLibx264 = strings.Contains(encoders, " libx264 ")
	caps.HasLibx265 = strings.Contains(encoders, " libx265 ")
	caps.HasLibaomAV1 = strings.Contains(encoders, " libaom-av1 ")
	caps.HasLibsvtAV1 = strings.Contains(encoders, " libsvtav1 ")
	caps.HasNvencH264 = strings.Contains(encoders, " h264_nvenc ")
	caps.HasNvencHEVC = strings.Contains(encoders, " hevc_nvenc ")
	caps.HasNvencAV1 = strings.Contains(encoders, " av1_nvenc ")
	caps.HasQSVH264 = strings.Contains(encoders, " h264_qsv ")
	caps.HasQSVHEVC = strings.Contains(encoders, " hevc_qsv ")
	caps.HasQSVAV1 = strings.Contains(encoders, " av1_qsv ")
	caps.HasAMFH264 = strings.Contains(encoders, " h264_amf ")
	caps.HasAMFHEVC = strings.Contains(encoders, " hevc_amf ")
	caps.HasAMFAV1 = strings.Contains(encoders, " av1_amf ")
	caps.HasVTH264 = strings.Contains(encoders, " h264_videotoolbox ")
	caps.HasVTHEVC = strings.Contains(encoders, " hevc_videotoolbox ")
	return caps
}

// codecFamily normalizes a target codec or concrete encoder name to its
// codec family: "hevc", "h264", or "av1".
func codecFamily(targetCodec string) string {
	c := strings.ToLower(targetCodec)
	switch {
	case strings.Contains(c, "265") || strings.Contains(c, "hevc"):
		return "hevc"
	case strings.Contains(c, "264"):
		return "h264"
	case strings.Contains(c, "av1"):
		return "av1"
	default:
		return ""
	}
}

// hasWorkingQSV reports whether Intel Quick Sync encode hardware is
// evidenced on this machine via any live canary that ran QSV encode successfully.
func (c EncoderCaps) hasWorkingQSV() bool {
	return c.HasQSVHEVCVerified || c.HasQSVH264Verified ||
		c.HasQSVHEVCTuningVerified || c.HasQSVH264TuningVerified ||
		c.HasQSVAV1Verified || c.HasQSVAV1TuningVerified ||
		c.HasQSVExtBRCVerified || c.HasQSVAdaptiveIBVerified
}

// ResolveEncoder maps a requested preset encoder engine ("cpu", "nvenc",
// "qsv") plus the target codec family to a concrete FFmpeg encoder that the
// probed caps verify. When the requested engine cannot encode that codec on
// this machine (no GPU, no encoder, or unverified AV1 hardware), it falls
// back to the agent's own tiered recommendation so a preset can never fail a
// job just because the target machine lacks the hardware. An empty request
// returns targetCodec unchanged (legacy behavior).
func ResolveEncoder(requested, targetCodec string, caps EncoderCaps) string {
	if requested == "" {
		return targetCodec
	}
	family := codecFamily(targetCodec)
	var encoder string

	hasNvenc := caps.HasNvidia || caps.HasNvencTuningVerified || caps.HasNvencAV1Verified
	hasQSV := caps.hasWorkingQSV()

	switch family {
	case "hevc":
		switch requested {
		case "cpu":
			if caps.HasLibx265 {
				encoder = "libx265"
			}
		case "nvenc":
			if caps.HasNvencHEVC && hasNvenc {
				encoder = "hevc_nvenc"
			}
		case "qsv":
			if caps.HasQSVHEVC && hasQSV {
				encoder = "hevc_qsv"
			}
		}
	case "h264":
		switch requested {
		case "cpu":
			if caps.HasLibx264 {
				encoder = "libx264"
			}
		case "nvenc":
			if caps.HasNvencH264 && hasNvenc {
				encoder = "h264_nvenc"
			}
		case "qsv":
			if caps.HasQSVH264 && hasQSV {
				encoder = "h264_qsv"
			}
		}
	case "av1":
		switch requested {
		case "cpu":
			if caps.HasLibsvtAV1 {
				encoder = "libsvtav1"
			} else if caps.HasLibaomAV1 {
				encoder = "libaom-av1"
			}
		case "nvenc":
			if caps.HasNvencAV1Verified {
				encoder = "av1_nvenc"
			}
		case "qsv":
			if caps.HasQSVAV1Verified {
				encoder = "av1_qsv"
			}
		}
	}

	if encoder == "" && requested != "cpu" && requested != "nvenc" && requested != "qsv" {
		log.Printf("unknown encoder engine %q requested; falling back to agent recommendation", requested)
	}
	if encoder != "" {
		return encoder
	}

	// Stay within the same codec family and fall back to CPU software encoding.
	var cpuFallback string
	switch family {
	case "av1":
		if caps.HasLibsvtAV1 {
			cpuFallback = "libsvtav1"
		} else if caps.HasLibaomAV1 {
			cpuFallback = "libaom-av1"
		}
	case "hevc":
		if caps.HasLibx265 {
			cpuFallback = "libx265"
		}
	case "h264":
		if caps.HasLibx264 {
			cpuFallback = "libx264"
		}
	}

	if cpuFallback != "" {
		engineLabel := requested
		switch requested {
		case "qsv":
			engineLabel = "Intel(QSV)"
		case "nvenc":
			engineLabel = "NVIDIA(NVENC)"
		}
		log.Printf("%s encode unsupported for %s, falling back to CPU (%s)", engineLabel, family, cpuFallback)
		return cpuFallback
	}

	if requested != "cpu" && requested != "nvenc" && requested != "qsv" {
		log.Printf("unknown encoder engine %q requested; falling back to agent recommendation", requested)
	}
	fallback := caps.RecommendedEncoder
	if fallback == "" {
		fallback, _, _, _ = deriveRecommendation(caps)
	}
	log.Printf("encoder engine %q not available for codec family %q on this machine; falling back to %s", requested, family, fallback)
	return fallback
}

// StripUnverified clears accelerator flags for any vendor where neither physical
// hardware presence nor live canary execution verified that the accelerator is functional.
func (caps *EncoderCaps) StripUnverified(hasNvidia, hasIntel, hasAMD bool) {
	caps.HasNvidia = hasNvidia
	caps.HasIntel = hasIntel
	caps.HasAMD = hasAMD

	hasNvenc := hasNvidia || caps.HasNvencTuningVerified || caps.HasNvencAV1Verified
	if !hasNvenc {
		caps.HasNvencH264 = false
		caps.HasNvencHEVC = false
		caps.HasNvencAV1 = false
		caps.HasHWAccelCUDA = false
	}

	hasQSV := caps.hasWorkingQSV()
	if !hasQSV {
		caps.HasQSVH264 = false
		caps.HasQSVHEVC = false
		caps.HasQSVAV1 = false
		caps.HasHWAccelQSV = false
	}

	hasAMF := hasAMD || caps.HasAMFTuningVerified || caps.HasAMFAV1Verified
	if !hasAMF {
		caps.HasAMFH264 = false
		caps.HasAMFHEVC = false
		caps.HasAMFAV1 = false
	}

	caps.RecommendedEncoder, caps.RecommendedPreset, caps.RecommendedCRF, caps.EstimatedFPS = deriveRecommendation(*caps)
}

// deriveRecommendation selects the best encoder for this system and returns
// a ready-to-use FFmpeg encoder name, preset, CRF, and estimated realtime FPS.
//
// Priority: NVENC > QSV > AMF > VideoToolbox > libsvtav1 > libx265 > libx264
//
// CRF/preset targets H.265 quality equivalent to H.264 CRF 20 (~visually lossless).
func deriveRecommendation(caps EncoderCaps) (encoder, preset string, crf int, fps float64) {
	hasNvenc := caps.HasNvidia || caps.HasNvencTuningVerified || caps.HasNvencAV1Verified
	hasQSV := caps.hasWorkingQSV()
	hasAMF := caps.HasAMD || caps.HasAMFTuningVerified || caps.HasAMFAV1Verified

	switch {
	case caps.HasNvencHEVC && hasNvenc:
		// NVENC: no CRF mode; use CQ (constant quality) 24 with p6 preset.
		// Approx 4–8x realtime on modern NVIDIA GPUs.
		return "hevc_nvenc", "p6", 24, 180.0
	case caps.HasNvencH264 && hasNvenc:
		return "h264_nvenc", "p4", 23, 240.0
	case caps.HasQSVHEVC && hasQSV:
		// Intel QSV: global_quality 25 analogous to CRF.
		return "hevc_qsv", "medium", 25, 120.0
	case caps.HasQSVH264 && hasQSV:
		return "h264_qsv", "medium", 23, 150.0
	case caps.HasAMFHEVC && hasAMF:
		return "hevc_amf", "balanced", 25, 100.0
	case caps.HasAMFH264 && hasAMF:
		return "h264_amf", "balanced", 23, 120.0
	case caps.HasVTHEVC:
		return "hevc_videotoolbox", "default", 0, 80.0
	case caps.HasVTH264:
		return "h264_videotoolbox", "default", 0, 100.0
	case caps.HasLibsvtAV1:
		// SVT-AV1: preset 6 is a good quality/speed balance; CRF 32 ≈ H.265 CRF 28.
		return "libsvtav1", "6", 32, 30.0
	case caps.HasLibx265:
		return "libx265", "medium", 28, 24.0
	case caps.HasLibx264:
		return "libx264", "medium", 23, 40.0
	default:
		return "libx265", "medium", 28, 0.0
	}
}
