//go:build !windows

package main

import (
	"fmt"
	"strings"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
)

type FileTranscodeProfile struct {
	Resolution  string
	HDR         string
	Duration    float64
	TotalFrames int
}

type ResolutionEstimate struct {
	Count         int
	TotalDuration float64
	TranscodeSecs float64
	AvgFPS        float64
}

type LibraryEstimate struct {
	TotalTranscodeSecs float64
	WeightedFPS        float64
	ByResolution       map[string]ResolutionEstimate
	HWNote             string
}

func isGPUAccelerated(hw hardware.Capabilities, enc ffmpeg.EncoderCaps) bool {
	if hw.GPU.HasNvidia || hw.GPU.HasIntel || hw.GPU.HasAMD {
		return true
	}
	if enc.HasNvencTuningVerified || enc.HasQSVHEVCTuningVerified || enc.HasQSVH264TuningVerified ||
		enc.HasAMFTuningVerified || enc.HasNvencAV1Verified || enc.HasQSVAV1Verified || enc.HasAMFAV1Verified {
		return true
	}
	return false
}

// EstimateFileFPS computes realistic transcode speeds (realtime FPS) based on
// hardware capabilities, hardware encoder/filter verification, and media attributes.
func EstimateFileFPS(hw hardware.Capabilities, enc ffmpeg.EncoderCaps, resolution, hdr string) float64 {
	hasGPU := isGPUAccelerated(hw, enc)

	isHDR := hdr != "" && strings.ToUpper(hdr) != "SDR"

	if hasGPU {
		genMultiplier := 1.0
		arch := strings.ToLower(hw.GPU.Architecture)
		switch {
		case strings.Contains(arch, "ada") || strings.Contains(arch, "rtx 40"):
			genMultiplier = 1.25
		case strings.Contains(arch, "pascal") || strings.Contains(arch, "maxwell") || strings.Contains(arch, "kepler") || strings.Contains(arch, "gcn"):
			genMultiplier = 0.75
		}

		switch resolution {
		case "4K":
			if isHDR {
				if enc.HasHWTonemapVerified {
					return 45.0 * genMultiplier
				}
				// Falls back to CPU software tone-mapping filter
				return 18.0
			}
			return 55.0 * genMultiplier
		case "1080p":
			if isHDR {
				return 110.0 * genMultiplier
			}
			return 160.0 * genMultiplier
		case "720p":
			return 260.0 * genMultiplier
		case "SD":
			return 320.0 * genMultiplier
		default:
			return 150.0 * genMultiplier
		}
	}

	// CPU software fallback
	if hw.CPU.HasAVX2 {
		switch resolution {
		case "4K":
			if isHDR {
				return 8.0
			}
			return 12.0
		case "1080p":
			return 24.0
		case "720p":
			return 45.0
		case "SD":
			return 75.0
		default:
			return 24.0
		}
	}

	// Underpowered CPU without AVX2
	const slowFactor = 0.125
	switch resolution {
	case "4K":
		return 8.0 * slowFactor
	case "1080p":
		return 24.0 * slowFactor // ~3 FPS
	case "720p":
		return 45.0 * slowFactor
	case "SD":
		return 75.0 * slowFactor
	default:
		return 24.0 * slowFactor
	}
}

// CalculateLibraryEstimate aggregates per-file resolution and HDR transcode time estimates.
func CalculateLibraryEstimate(hw hardware.Capabilities, enc ffmpeg.EncoderCaps, files []FileTranscodeProfile) LibraryEstimate {
	byRes := make(map[string]ResolutionEstimate)
	totalTranscodeSecs := 0.0
	totalFramesAcc := 0.0

	for _, f := range files {
		resKey := f.Resolution
		if resKey == "" || resKey == "unknown" {
			resKey = "1080p"
		}

		fps := EstimateFileFPS(hw, enc, resKey, f.HDR)
		if fps <= 0 {
			fps = 24.0
		}

		frames := float64(f.TotalFrames)
		if frames <= 0 {
			frames = f.Duration * 24.0
		}

		transcodeSecs := frames / fps
		totalTranscodeSecs += transcodeSecs
		totalFramesAcc += frames

		curr := byRes[resKey]
		curr.Count++
		curr.TotalDuration += f.Duration
		curr.TranscodeSecs += transcodeSecs
		byRes[resKey] = curr
	}

	// Calculate average FPS per resolution
	for k, v := range byRes {
		if v.TranscodeSecs > 0 {
			frames := v.TotalDuration * 24.0
			v.AvgFPS = frames / v.TranscodeSecs
			byRes[k] = v
		}
	}

	weightedFPS := 0.0
	if totalTranscodeSecs > 0 {
		weightedFPS = totalFramesAcc / totalTranscodeSecs
	}

	hasGPU := isGPUAccelerated(hw, enc)

	hwNote := fmt.Sprintf("(GPU acceleration ~%.0f FPS weighted avg)", weightedFPS)
	if !hasGPU {
		if hw.CPU.HasAVX2 {
			hwNote = fmt.Sprintf("(CPU software encoding ~%.0f FPS weighted avg)", weightedFPS)
		} else {
			hwNote = fmt.Sprintf("(CPU underpowered ~%.0f FPS weighted avg)", weightedFPS)
		}
	}

	return LibraryEstimate{
		TotalTranscodeSecs: totalTranscodeSecs,
		WeightedFPS:        weightedFPS,
		ByResolution:       byRes,
		HWNote:             hwNote,
	}
}
