package ffmpeg

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

//go:embed models/vmaf_v0.6.1.json
var defaultVMAFModelJSON []byte

// VMAF spot-check bounds and quality floors per encoding presets and ADRs.
const (
	// VMAFSampleDurationSecs caps how much content is compared (60s spot-check).
	VMAFSampleDurationSecs = 60.0
	// VMAFSampleOffsetSecs starts the sample this far into the source (30s offset),
	// past container-header and encoder-priming edge cases at t=0.
	VMAFSampleOffsetSecs = 30.0
	// VMAFFloorArchivist is the quality floor for archival tiers (~95-96).
	VMAFFloorArchivist = 95.0
	// VMAFFloorBalanced is the quality floor for balanced tiers (~92-93).
	VMAFFloorBalanced = 92.0
	// VMAFDefaultWarningFloor is the fallback warning floor.
	VMAFDefaultWarningFloor = 92.0
)

// QualityFloor returns the VMAF quality floor for a given tier or preset name.
func QualityFloor(tierOrName string) float64 {
	return QualityFloorForPreset(tierOrName)
}

// QualityFloorForPreset returns the VMAF quality floor for a given preset name.
func QualityFloorForPreset(name string) float64 {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "archivist") {
		return VMAFFloorArchivist
	}
	if strings.Contains(lower, "balanced") {
		return VMAFFloorBalanced
	}
	return VMAFDefaultWarningFloor
}

var vmafScorePattern = regexp.MustCompile(`(?:VMAF score|vmaf):\s*([0-9.]+)`)

// MeasureVMAF runs a bounded ffmpeg VMAF comparison between origPath and
// outPath, decoding `duration` seconds from `offset` on both inputs, and
// returns the harmonic mean VMAF score. A non-nil error means the score could not be
// produced (ffmpeg build without libvmaf filter, unreadable input, output
// that fails to decode): callers treat that as a skip, not a failure.
func MeasureVMAF(ctx context.Context, ffmpegPath, origPath, outPath string, offset, duration float64) (float64, error) {
	ss := strconv.FormatFloat(offset, 'f', 3, 64)
	dur := strconv.FormatFloat(duration, 'f', 3, 64)

	// In libvmaf, input 0 is distorted (encoded output) and input 1 is reference (original).
	modelOpt := "version=vmaf_v0.6.1"
	modelPaths := []string{
		filepath.Join(filepath.Dir(ffmpegPath), "vmaf_v0.6.1.json"),
		filepath.Join(filepath.Dir(ffmpegPath), "models", "vmaf_v0.6.1.json"),
		"/usr/local/share/model/vmaf_v0.6.1.json",
		"/usr/share/model/vmaf_v0.6.1.json",
	}
	for _, mp := range modelPaths {
		if fi, err := os.Stat(mp); err == nil && !fi.IsDir() {
			modelOpt = fmt.Sprintf("path=%s", mp)
			break
		}
	}

	filter := fmt.Sprintf("libvmaf=model='%s':n_subsample=5:n_threads=4:pool=harmonic_mean", modelOpt)

	args := []string{
		"-nostdin",
		"-nostats",
		"-ss", ss,
		"-t", dur,
		"-i", outPath,
		"-ss", ss,
		"-t", dur,
		"-i", origPath,
		"-filter_complex", fmt.Sprintf("[0:v][1:v]scale2ref[main][ref];[main][ref]%s", filter),
		"-f", "null",
		"-",
	}
	cmd := execCommandContext(ctx, ffmpegPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("vmaf comparison failed: %v: %s", err, tailForError(string(out), demuxErrorTailBytes))
	}
	m := vmafScorePattern.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("vmaf comparison produced no score: %s", tailForError(string(out), demuxErrorTailBytes))
	}
	score, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		return 0, fmt.Errorf("parsing vmaf score: %w", err)
	}
	return score, nil
}
