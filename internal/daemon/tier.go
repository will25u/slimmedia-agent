package daemon

import (
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
)

// HardwareTier represents the classification tier of the machine for transcoding.
type HardwareTier string

const (
	TierExcellent      HardwareTier = "Excellent"
	TierGood           HardwareTier = "Good"
	TierBasic          HardwareTier = "Basic"
	TierNotRecommended HardwareTier = "Not Recommended"
)

// TierAssessment holds the classified tier and explanation message.
type TierAssessment struct {
	Tier    HardwareTier
	Message string
}

// DetermineTier classifies the machine for transcoding:
//
//	Excellent       - AV1 hardware encode verified by a canary encode on this
//	                  machine (any vendor, discrete or integrated)
//	Good            - GPU present with an H.264/HEVC hardware encoder, but no
//	                  AV1 encode
//	Basic           - software encoding only (AVX2 CPU)
//	Not Recommended - neither
func DetermineTier(caps hardware.Capabilities, encCaps ffmpeg.EncoderCaps) TierAssessment {
	hasGPU := caps.GPU.HasNvidia || caps.GPU.HasIntel || caps.GPU.HasAMD
	hasVerifiedAV1 := encCaps.HasNvencAV1Verified || encCaps.HasQSVAV1Verified || encCaps.HasAMFAV1Verified
	// H.264/HEVC hardware encoders (presence in the FFmpeg binary is enough
	// here: no AV1-encode ASIC lacks HEVC, and AV1 is handled above).
	hasHwEncoder := encCaps.HasNvencHEVC || encCaps.HasQSVHEVC || encCaps.HasAMFHEVC || encCaps.HasVTHEVC

	if hasVerifiedAV1 {
		return TierAssessment{
			Tier:    TierExcellent,
			Message: "Your system is excellent for hardware transcoding (AV1 encode verified).",
		}
	}

	if hasGPU || encCaps.HasVTH264 || encCaps.HasVTHEVC {
		if hasHwEncoder {
			return TierAssessment{
				Tier:    TierGood,
				Message: "Your system is good for hardware transcoding. No AV1 encode support.",
			}
		}
		return TierAssessment{
			Tier:    TierGood,
			Message: "Your system is capable. Performance may be moderate.",
		}
	}

	if caps.CPU.HasAVX2 {
		return TierAssessment{
			Tier:    TierBasic,
			Message: "Software encoding only. Transcoding will be slow but functional.",
		}
	}

	return TierAssessment{
		Tier:    TierNotRecommended,
		Message: "This system is not powerful enough for media transcoding.",
	}
}
