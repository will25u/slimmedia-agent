//go:build !windows

package main

import (
	"testing"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
)

func TestEstimateFileFPS_GPU(t *testing.T) {
	hw := hardware.Capabilities{}
	hw.GPU.HasNvidia = true
	hw.GPU.Architecture = "Ampere"

	enc := ffmpeg.EncoderCaps{
		HasNvencHEVC:              true,
		HasHWTonemapVerified:      true,
		HasHWDecodeHEVC10Verified: true,
	}

	// 4K HDR with HW tonemap
	fps4KHDR := EstimateFileFPS(hw, enc, "4K", "HDR10")
	if fps4KHDR < 35 || fps4KHDR > 55 {
		t.Errorf("expected 4K HDR FPS between 35 and 55, got %.1f", fps4KHDR)
	}

	// 4K HDR without HW tonemap (software filter bottleneck)
	encNoTonemap := enc
	encNoTonemap.HasHWTonemapVerified = false
	fps4KNoTonemap := EstimateFileFPS(hw, encNoTonemap, "4K", "HDR10")
	if fps4KNoTonemap >= fps4KHDR {
		t.Errorf("expected CPU tonemap fallback FPS (%.1f) < HW tonemap FPS (%.1f)", fps4KNoTonemap, fps4KHDR)
	}

	// 1080p SDR
	fps1080p := EstimateFileFPS(hw, enc, "1080p", "SDR")
	if fps1080p < 120 || fps1080p > 200 {
		t.Errorf("expected 1080p SDR FPS between 120 and 200, got %.1f", fps1080p)
	}

	// 720p / SD
	fps720p := EstimateFileFPS(hw, enc, "720p", "SDR")
	if fps720p < 220 {
		t.Errorf("expected 720p FPS >= 220, got %.1f", fps720p)
	}
}

func TestEstimateFileFPS_CPUFallback(t *testing.T) {
	// Modern CPU with AVX2
	hwAVX2 := hardware.Capabilities{}
	hwAVX2.CPU.HasAVX2 = true
	encCPU := ffmpeg.EncoderCaps{
		HasLibx265: true,
	}

	fpsAVX2_1080p := EstimateFileFPS(hwAVX2, encCPU, "1080p", "SDR")
	if fpsAVX2_1080p < 20 || fpsAVX2_1080p > 28 {
		t.Errorf("expected AVX2 1080p FPS ~24, got %.1f", fpsAVX2_1080p)
	}

	// Underpowered CPU without AVX2
	hwSlow := hardware.Capabilities{}
	hwSlow.CPU.HasAVX2 = false
	fpsSlow := EstimateFileFPS(hwSlow, encCPU, "1080p", "SDR")
	if fpsSlow < 2 || fpsSlow > 5 {
		t.Errorf("expected underpowered CPU 1080p FPS ~3, got %.1f", fpsSlow)
	}
}

func TestCalculateLibraryEstimate(t *testing.T) {
	hw := hardware.Capabilities{}
	hw.GPU.HasNvidia = true
	enc := ffmpeg.EncoderCaps{
		HasNvencHEVC:         true,
		HasHWTonemapVerified: true,
	}

	files := []FileTranscodeProfile{
		{Resolution: "4K", HDR: "HDR10", Duration: 3600, TotalFrames: 86400},
		{Resolution: "1080p", HDR: "SDR", Duration: 3600, TotalFrames: 86400},
	}

	estimate := CalculateLibraryEstimate(hw, enc, files)
	if estimate.TotalTranscodeSecs <= 0 {
		t.Errorf("expected TotalTranscodeSecs > 0, got %.1f", estimate.TotalTranscodeSecs)
	}

	est4K, ok4K := estimate.ByResolution["4K"]
	est1080p, ok1080p := estimate.ByResolution["1080p"]
	if !ok4K || !ok1080p {
		t.Fatalf("expected resolution breakdowns for 4K and 1080p; got %+v", estimate.ByResolution)
	}

	if est4K.Count != 1 || est1080p.Count != 1 {
		t.Errorf("unexpected counts: 4K=%d, 1080p=%d", est4K.Count, est1080p.Count)
	}

	// 4K should take longer than 1080p for same duration
	if est4K.TranscodeSecs <= est1080p.TranscodeSecs {
		t.Errorf("expected 4K transcode time (%.1fs) > 1080p transcode time (%.1fs)", est4K.TranscodeSecs, est1080p.TranscodeSecs)
	}
}
