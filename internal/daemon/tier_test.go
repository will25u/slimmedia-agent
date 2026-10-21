package daemon

import (
	"testing"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
)

func TestDetermineTier(t *testing.T) {
	tests := []struct {
		name         string
		caps         hardware.Capabilities
		encCaps      ffmpeg.EncoderCaps
		expectedTier HardwareTier
	}{
		{
			name:         "Excellent tier - verified NVENC AV1 encode",
			caps:         capsWithNvidia(),
			encCaps:      ffmpeg.EncoderCaps{HasNvencAV1: true, HasNvencAV1Verified: true},
			expectedTier: TierExcellent,
		},
		{
			name:         "Excellent tier - verified QSV AV1 encode",
			caps:         capsWithIntel(),
			encCaps:      ffmpeg.EncoderCaps{HasQSVAV1: true, HasQSVAV1Verified: true},
			expectedTier: TierExcellent,
		},
		{
			name:         "Excellent tier - verified AMF AV1 encode",
			caps:         capsWithAMD(),
			encCaps:      ffmpeg.EncoderCaps{HasAMFAV1: true, HasAMFAV1Verified: true},
			expectedTier: TierExcellent,
		},
		{
			name:         "Good tier - discrete GPU present with H.264/HEVC hardware encoder",
			caps:         capsWithNvidia(),
			encCaps:      ffmpeg.EncoderCaps{HasNvencHEVC: true},
			expectedTier: TierGood,
		},
		{
			name:         "Good tier - discrete GPU present but lacks hardware encoders (capable fallback)",
			caps:         capsWithNvidia(),
			encCaps:      ffmpeg.EncoderCaps{},
			expectedTier: TierGood,
		},
		{
			name: "Good tier - macOS VideoToolbox supported",
			caps: hardware.Capabilities{},
			encCaps: ffmpeg.EncoderCaps{
				HasVTH264: true,
				HasVTHEVC: true,
			},
			expectedTier: TierGood,
		},
		{
			name:         "Basic tier - AVX2 CPU software transcoding only",
			caps:         capsWithAVX2(true),
			encCaps:      ffmpeg.EncoderCaps{},
			expectedTier: TierBasic,
		},
		{
			name:         "Not Recommended tier - no hardware encoding and no AVX2",
			caps:         capsWithAVX2(false),
			encCaps:      ffmpeg.EncoderCaps{},
			expectedTier: TierNotRecommended,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assessment := DetermineTier(tt.caps, tt.encCaps)
			if assessment.Tier != tt.expectedTier {
				t.Errorf("expected %s, got %s", tt.expectedTier, assessment.Tier)
			}
			if assessment.Message == "" {
				t.Errorf("expected non-empty explanation message")
			}
		})
	}
}

func capsWithNvidia() hardware.Capabilities {
	var c hardware.Capabilities
	c.GPU.HasNvidia = true
	return c
}

func capsWithIntel() hardware.Capabilities {
	var c hardware.Capabilities
	c.GPU.HasIntel = true
	return c
}

func capsWithAMD() hardware.Capabilities {
	var c hardware.Capabilities
	c.GPU.HasAMD = true
	return c
}

func capsWithAVX2(has bool) hardware.Capabilities {
	var c hardware.Capabilities
	c.CPU.HasAVX2 = has
	return c
}

