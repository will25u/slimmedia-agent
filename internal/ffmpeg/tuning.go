package ffmpeg

import (
	"log"
	"strconv"
	"strings"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// hwTuningFlagSets holds each hardware HEVC encoder's default advanced
// tuning-flag argv. The canary probe (verifyTuningFlags) encodes with
// exactly these flags so a driver that passes the canary accepts the whole
// package; buildFfmpegArgs emits these flags unless the job carries a
// user-selected vendor tuning (issue #195), which replaces the package
// field-for-field — every selectable flag is a member of this package, so
// the canary still covers it.
var hwTuningFlagSets = map[string][]string{
	"hevc_qsv":   {"-extbrc", "1", "-look_ahead", "1", "-look_ahead_depth", "40", "-b_strategy", "1", "-adaptive_b", "1", "-adaptive_i", "1"},
	"h264_qsv":   {"-extbrc", "1", "-b_strategy", "1"},
	"av1_qsv":    {"-extbrc", "1", "-b_strategy", "1"},
	"hevc_nvenc": {"-tune", "hq", "-multipass", "fullres", "-spatial-aq", "1", "-temporal-aq", "1", "-b_ref_mode", "middle"},
	"av1_nvenc":  {"-tune", "hq", "-multipass", "fullres", "-spatial-aq", "1", "-b_ref_mode", "middle"},
	"hevc_amf":   {"-quality", "quality", "-b_frame_delta_qp", "4", "-vbaq", "true"},
}

// defaultVendorFlagValue returns the value of flag in the encoder's default
// tuning package, or "" when the flag is not part of the package.
func defaultVendorFlagValue(encoder, flag string) string {
	pkg := hwTuningFlagSets[encoder]
	for i := 0; i+1 < len(pkg); i++ {
		if pkg[i] == flag {
			return pkg[i+1]
		}
	}
	return ""
}

// vendorTuningArgsFor renders a user-selected vendor tuning (issue #195) as
// argv for the resolved encoder, falling back per field to the encoder's
// default package from hwTuningFlagSets when a field is unset; an explicit
// false boolean omits that flag. It returns nil for encoders whose vendor
// package the selection cannot map to (amf, ...), where the caller keeps the
// default package instead.
func vendorTuningArgsFor(encoder string, vt *protocol.VendorTuning) []string {
	if vt == nil {
		return nil
	}
	switch encoder {
	case "hevc_nvenc", "av1_nvenc":
		tune := defaultVendorFlagValue(encoder, "-tune")
		if vt.Tune != nil {
			tune = *vt.Tune
		}
		multipass := defaultVendorFlagValue(encoder, "-multipass")
		if vt.Multipass != nil {
			multipass = *vt.Multipass
		}
		bRefMode := defaultVendorFlagValue(encoder, "-b_ref_mode")
		if vt.BRefMode != nil {
			bRefMode = *vt.BRefMode
		}
		args := []string{"-tune", tune, "-multipass", multipass}
		if vt.SpatialAQ == nil || *vt.SpatialAQ {
			args = append(args, "-spatial-aq", "1")
		}
		if encoder != "av1_nvenc" {
			if vt.TemporalAQ == nil || *vt.TemporalAQ {
				args = append(args, "-temporal-aq", "1")
			}
		} else if vt.TemporalAQ != nil && *vt.TemporalAQ {
			args = append(args, "-temporal-aq", "1")
		}
		return append(args, "-b_ref_mode", bRefMode)
	case "hevc_qsv":
		return qsvVendorTuningArgs(encoder, vt, true)
	case "h264_qsv", "av1_qsv":
		return qsvVendorTuningArgs(encoder, vt, false)
	}
	return nil
}

// vendorTuningSelectionForeign reports whether vt sets fields of the other
// hardware vendor family (NVENC fields vs QSV fields) — e.g. an NVENC preset
// whose job resolved to QSV on a fallback agent. Used only for an honest log;
// the resolved encoder's defaults apply either way.
func vendorTuningSelectionForeign(encoder string, vt *protocol.VendorTuning) bool {
	nvencField := vt.Tune != nil || vt.Multipass != nil || vt.SpatialAQ != nil || vt.TemporalAQ != nil || vt.BRefMode != nil
	qsvField := vt.Extbrc != nil || vt.LookAhead != nil || vt.LookAheadDepth != nil || vt.BStrategy != nil || vt.AdaptiveB != nil || vt.AdaptiveI != nil
	switch {
	case strings.HasSuffix(encoder, "_qsv"):
		return nvencField
	case strings.HasSuffix(encoder, "_nvenc"):
		return qsvField
	}
	return false
}

// qsvVendorTuningArgs renders the QSV tuning selection; the H.264/AV1 QSV
// encoders accept only the reduced set (no lookahead, no adaptive frames).
func qsvVendorTuningArgs(encoder string, vt *protocol.VendorTuning, full bool) []string {
	var args []string
	if vt.Extbrc == nil || *vt.Extbrc {
		args = append(args, "-extbrc", "1")
	}
	if full && (vt.LookAhead == nil || *vt.LookAhead) {
		depth := 40
		if d := defaultVendorFlagValue(encoder, "-look_ahead_depth"); d != "" {
			if n, err := strconv.Atoi(d); err == nil {
				depth = n
			}
		}
		if vt.LookAheadDepth != nil {
			depth = *vt.LookAheadDepth
		}
		args = append(args, "-look_ahead", "1", "-look_ahead_depth", strconv.Itoa(depth))
	}
	if vt.BStrategy == nil || *vt.BStrategy {
		args = append(args, "-b_strategy", "1")
	}
	if full {
		if vt.AdaptiveB == nil || *vt.AdaptiveB {
			args = append(args, "-adaptive_b", "1")
		}
		if vt.AdaptiveI == nil || *vt.AdaptiveI {
			args = append(args, "-adaptive_i", "1")
		}
	}
	return args
}

// archivistPackage owns the Archivist-grade parameters for one resolved
// encoder. These values are code-owned: the Archivist preset's own
// CRF/preset-speed fields never reach the argv for these encoders, so the
// job holds Archivist grade no matter which hardware fallback step resolved.
//
//   - NVENC: constant quality 20 via VBR rate control with zero target
//     bitrate (published measurements show zero quality gain below CQ 20),
//     at the slowest speed preset (p7).
//   - QSV: ICQ quality 13 (constant quality; near-lossless grain and edge
//     retention), at the slower speed preset.
//   - CPU: exactly the historical software Archivist, CRF 18 at slow.
type archivistPackage struct {
	quality []string
	speed   string
}

var archivistPackages = map[string]archivistPackage{
	"hevc_nvenc": {quality: []string{"-rc:v", "vbr", "-cq:v", "20", "-b:v", "0"}, speed: "p7"},
	"av1_nvenc":  {quality: []string{"-rc:v", "vbr", "-cq:v", "24", "-b:v", "0"}, speed: "p7"},
	"hevc_qsv":   {quality: []string{"-rc", "icq", "-global_quality", "13"}, speed: "slower"},
	"libx265":    {quality: []string{"-crf", "18"}, speed: "slow"},
	"libsvtav1":  {quality: []string{"-crf", "22"}, speed: "4"},
}

// applyArchivistGrade replaces the builder's quality, speed, and vendor
// tuning arguments with the code-owned Archivist package for the resolved
// encoder. Every tuning flag is gated on this machine's canary
// verification and stripped (with a log) when the driver proved it cannot
// accept it — never silently dropped, never failing the job. It returns
// false when the resolved encoder has no Archivist package (the preset's
// own values apply unchanged).
func applyArchivistGrade(b *CommandBuilder, encoder string, caps EncoderCaps) bool {
	pkg, ok := archivistPackages[encoder]
	if !ok {
		return false
	}
	b.videoQualityArgs = append([]string{}, pkg.quality...)
	if pkg.speed != "" {
		b.videoPreset = pkg.speed
	}

	switch encoder {
	case "hevc_nvenc", "av1_nvenc":
		// Full quality-tuning package: high-quality tune, fullres
		// multipass, spatial adaptive quantization, middle B-frame
		// reference mode. Same argv the canary proved.
		if tuningVerified(encoder, caps) {
			b.SetVendorTuningArgs(hwTuningFlagSets[encoder])
		} else {
			log.Printf("archivist grade: stripping %s tuning flags (canary encode failed; driver rejects advanced flags)", encoder)
			b.vendorTuningArgs = nil
		}
	case "hevc_qsv":
		// Extended bitrate control, plus adaptive I/B frames only when
		// the per-driver canary proved them. No lookahead under ICQ.
		var tuning []string
		if caps.HasQSVExtBRCVerified {
			tuning = append(tuning, "-extbrc", "1")
		} else {
			log.Printf("archivist grade: stripping hevc_qsv -extbrc (canary encode failed; driver rejects extended bitrate control)")
		}
		if caps.HasQSVAdaptiveIBVerified {
			tuning = append(tuning, "-adaptive_i", "1", "-adaptive_b", "1")
		} else {
			log.Printf("archivist grade: stripping hevc_qsv -adaptive_i/-adaptive_b (canary encode failed; driver rejects adaptive frames)")
		}
		b.SetVendorTuningArgs(tuning)
	default:
		// libx265 and any future software entry: no vendor tuning block.
		b.vendorTuningArgs = nil
	}
	return true
}

// tuningVerified reports whether the canary encode proved this driver's
// acceptance of the encoder's tuning flag set.
func tuningVerified(encoder string, caps EncoderCaps) bool {
	switch encoder {
	case "hevc_qsv":
		return caps.HasQSVHEVCTuningVerified
	case "h264_qsv":
		return caps.HasQSVH264TuningVerified
	case "av1_qsv":
		return caps.HasQSVAV1TuningVerified
	case "hevc_nvenc":
		return caps.HasNvencTuningVerified
	case "av1_nvenc":
		return caps.HasNvencAV1TuningVerified
	case "hevc_amf":
		return caps.HasAMFTuningVerified
	}
	return false
}
