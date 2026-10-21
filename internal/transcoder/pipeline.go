package transcoder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/will25u/slimmedia-agent/internal/hardware"
	"github.com/will25u/slimmedia-agent/internal/jobpaths"
)

// PlacementProgressCallback receives live byte and percentage progress events
// during post-transcode placement and source disposition.
type PlacementProgressCallback func(subPhase, activity string, transferred, total int64, pct float64)

type progressReader struct {
	r        io.Reader
	total    int64
	read     int64
	subPhase string
	activity string
	cb       PlacementProgressCallback
	throttle time.Duration
	lastEmit time.Time
}

func newProgressReader(r io.Reader, total int64, subPhase, activity string, cb PlacementProgressCallback) *progressReader {
	pr := &progressReader{
		r:        r,
		total:    total,
		subPhase: subPhase,
		activity: activity,
		cb:       cb,
		throttle: 100 * time.Millisecond,
		lastEmit: time.Now(),
	}
	if cb != nil {
		cb(subPhase, activity, 0, total, 0.0)
	}
	return pr
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.read += int64(n)
		if pr.cb != nil {
			now := time.Now()
			pct := float64(0)
			if pr.total > 0 {
				pct = math.Min(100.0, float64(pr.read)/float64(pr.total)*100.0)
			}
			if pr.read >= pr.total || now.Sub(pr.lastEmit) >= pr.throttle {
				pr.lastEmit = now
				pr.cb(pr.subPhase, pr.activity, pr.read, pr.total, pct)
			}
		}
	}
	return n, err
}

func (pr *progressReader) finish() {
	if pr.cb != nil {
		pr.cb(pr.subPhase, pr.activity, pr.total, pr.total, 100.0)
	}
}

// Pipeline handles the file movement and verification sequence.
type Pipeline struct {
	ScratchDir    string
	QuarantineDir string
	MaxQuarantine uint64
}

func NewPipeline(scratchDir, quarantineDir string, maxQuarantine uint64) *Pipeline {
	os.MkdirAll(scratchDir, 0755)
	os.MkdirAll(quarantineDir, 0755)
	return &Pipeline{
		ScratchDir:    scratchDir,
		QuarantineDir: quarantineDir,
		MaxQuarantine: maxQuarantine,
	}
}

func hashFileWithProgress(path, subPhase, activity string, cb PlacementProgressCallback) (string, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	total := info.Size()
	pr := newProgressReader(f, total, subPhase, activity, cb)
	h := sha256.New()
	if _, err := io.Copy(h, pr); err != nil {
		return "", err
	}
	pr.finish()
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashFileNoFollow returns the SHA-256 of the file at path, refusing to
// follow a symlink final component.
func hashFileNoFollow(path string) (string, error) {
	return hashFileWithProgress(path, "", "", nil)
}

func verifyFileHashWithProgress(path, want, subPhase, activity string, cb PlacementProgressCallback) error {
	got, err := hashFileWithProgress(path, subPhase, activity, cb)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("sha256 mismatch for %s: computed %s, expected %s", path, got, want)
	}
	return nil
}

// VerifyFileHash returns nil only if the file at path hashes to want.
func VerifyFileHash(path, want string) error {
	return verifyFileHashWithProgress(path, want, "", "", nil)
}

func copyAndHashWithProgress(src, dst, subPhase, activity string, cb PlacementProgressCallback) (string, os.FileInfo, error) {
	in, err := openNoFollow(src)
	if err != nil {
		return "", nil, fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("stat source: %w", err)
	}

	out, err := createNoFollow(dst)
	if err != nil {
		return "", nil, fmt.Errorf("create destination: %w", err)
	}

	total := info.Size()
	pr := newProgressReader(in, total, subPhase, activity, cb)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), pr); err != nil {
		out.Close()
		return "", nil, fmt.Errorf("copy: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return "", nil, fmt.Errorf("sync destination: %w", err)
	}
	pr.finish()
	return hex.EncodeToString(h.Sum(nil)), info, out.Close()
}

// copyAndHash copies src to dst without following symlinks on either final
// component and returns the SHA-256 of the bytes copied. The destination is
// fsynced before close.
func copyAndHash(src, dst string) (string, os.FileInfo, error) {
	return copyAndHashWithProgress(src, dst, "", "", nil)
}

// CopyFile copies a file and returns an error if it fails. Symlinks on
// either final component are refused.
func (p *Pipeline) CopyFile(src, dst string) error {
	_, _, err := copyAndHash(src, dst)
	return err
}

// CopyFileWithProgress copies a file and reports live byte progress.
func (p *Pipeline) CopyFileWithProgress(src, dst, subPhase, activity string, cb PlacementProgressCallback) error {
	_, _, err := copyAndHashWithProgress(src, dst, subPhase, activity, cb)
	return err
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	units := "KMGTPE"
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit && exp < len(units)-1; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), units[exp])
}

// VerifySize checks if the new file is between 5% and 110% of the original
// size. It is a coarse sanity gate only; ReplaceSequence verifies content
// hashes before anything is deleted.
func (p *Pipeline) VerifySize(origPath, newPath string) error {
	// Lstat so a swapped-in symlink fails the ratio check instead of being
	// followed.
	origStat, err := os.Lstat(origPath)
	if err != nil {
		return err
	}

	if origStat.Size() == 0 {
		return fmt.Errorf("size verification failed: original file is 0 bytes")
	}

	newStat, err := os.Lstat(newPath)
	if err != nil {
		return err
	}

	if newStat.Size() == 0 {
		return fmt.Errorf("size verification failed: new file is 0 bytes")
	}

	ratio := float64(newStat.Size()) / float64(origStat.Size())
	if ratio > 1.10 {
		return fmt.Errorf("size verification failed: transcoded file (%s) is larger than original (%s) at %.0f%% of original size (must be between 5%% and 110%%; rejected to avoid increasing file size)",
			formatBytes(newStat.Size()), formatBytes(origStat.Size()), ratio*100)
	}
	if ratio < 0.05 {
		return fmt.Errorf("size verification failed: transcoded file (%s) is suspiciously small compared to original (%s) at %.1f%% of original size (must be between 5%% and 110%%; rejected to prevent corrupted output)",
			formatBytes(newStat.Size()), formatBytes(origStat.Size()), ratio*100)
	}

	return nil
}

const criticalDiskBytes = 50 * 1024 * 1024 * 1024

func resolveExistingDir(path string) string {
	clean := filepath.Clean(path)
	for {
		info, err := os.Stat(clean)
		if err == nil {
			if info.IsDir() {
				return clean
			}
			return filepath.Dir(clean)
		}
		parent := filepath.Dir(clean)
		if parent == clean {
			return clean
		}
		clean = parent
	}
}

// CheckBudget checks if the quarantine directory exceeds its budget or if disk space is critical
// across both the transcode scratch volume and destination volume.
func (p *Pipeline) CheckBudget(destDirs ...string) error {
	destDir := p.QuarantineDir
	if len(destDirs) > 0 && destDirs[0] != "" {
		destDir = destDirs[0]
	}

	// 1. Check scratch volume free disk space
	if p.ScratchDir != "" {
		scratchDir := resolveExistingDir(p.ScratchDir)
		freeScratch, err := hardware.DiskFreeBytes(scratchDir)
		if err != nil {
			return fmt.Errorf("failed to check scratch disk space: %w", err)
		}
		if freeScratch < criticalDiskBytes {
			return fmt.Errorf("critical disk space: only %d bytes available on scratch volume", freeScratch)
		}
	}

	// 2. Check destination volume free disk space
	if destDir != "" {
		destCheckDir := resolveExistingDir(destDir)
		freeDest, err := hardware.DiskFreeBytes(destCheckDir)
		if err != nil {
			return fmt.Errorf("failed to check destination disk space: %w", err)
		}
		if freeDest < criticalDiskBytes {
			return fmt.Errorf("critical disk space: only %d bytes available on destination volume", freeDest)
		}
	}

	// 3. Calculate quarantine size
	if p.QuarantineDir != "" {
		var quarantineSize uint64
		filepath.Walk(p.QuarantineDir, func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				quarantineSize += uint64(info.Size())
			}
			return nil
		})

		if p.MaxQuarantine > 0 && quarantineSize > p.MaxQuarantine {
			return fmt.Errorf("quarantine budget exceeded: %d > %d", quarantineSize, p.MaxQuarantine)
		}
	}

	return nil
}

// ApplyPlacementAndDisposition places the transcoded scratch output according to
// outputStrategy ("replace", "alongside", "alternate") and applies the post-transcode
// source lifecycle policy according to actionOnSuccess ("keep", "delete", "trash"/"archive").
// Returns the final path of the transcoded output.
func (p *Pipeline) ApplyPlacementAndDisposition(paths jobpaths.JobPaths, outputStrategy, actionOnSuccess, container string, onProgress ...PlacementProgressCallback) (string, error) {
	var cb PlacementProgressCallback
	if len(onProgress) > 0 {
		cb = onProgress[0]
	}

	if outputStrategy == "" {
		outputStrategy = "replace"
	}
	if actionOnSuccess == "" {
		actionOnSuccess = "keep"
	}

	if outputStrategy != "replace" && outputStrategy != "alongside" && outputStrategy != "alternate" {
		return "", fmt.Errorf("unsupported output file strategy: %q", outputStrategy)
	}
	if actionOnSuccess != "keep" && actionOnSuccess != "delete" && actionOnSuccess != "trash" && actionOnSuccess != "archive" && actionOnSuccess != "retain" {
		return "", fmt.Errorf("unsupported action on success: %q", actionOnSuccess)
	}

	origPath := paths.Source()
	scratchPath := paths.Scratch()
	destPath, err := paths.DestinationPath(outputStrategy, container)
	if err != nil {
		return "", fmt.Errorf("invalid destination path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create destination directory: %w", err)
	}

	if outputStrategy == "replace" {
		// Step 3: QUARANTINE — stream the original into its namespaced slot,
		// hashing the bytes as they are read from the open descriptor.
		quarantineDest := paths.Quarantine()
		origHash, origInfo, err := copyAndHashWithProgress(origPath, quarantineDest, "quarantine_backup", "Backing up original", cb)
		if err != nil {
			os.Remove(quarantineDest)
			return "", fmt.Errorf("step 3 failed (quarantine copy): %w", err)
		}

		// Step 4: VERIFY COPY — the quarantined bytes must hash to exactly what
		// was read from the original.
		if err := verifyFileHashWithProgress(quarantineDest, origHash, "quarantine_verify", "Verifying backup integrity", cb); err != nil {
			os.Remove(quarantineDest)
			return "", fmt.Errorf("step 4 failed (verify quarantine copy): %w", err)
		}

		// Step 5: DELETE ORIG — unlink the directory entry.
		currInfo, err := os.Lstat(origPath)
		if err != nil {
			return "", fmt.Errorf("step 5 failed (stat original): %w", err)
		}
		if !os.SameFile(origInfo, currInfo) {
			return "", fmt.Errorf("step 5 failed (delete original): file swapped before deletion")
		}
		if err := os.Remove(origPath); err != nil {
			return "", fmt.Errorf("step 5 failed (delete original): %w", err)
		}

		// Step 6: PLACE NEW — hash the transcoded output, then move it over the
		// original.
		scratchHash, err := hashFileWithProgress(scratchPath, "scratch_verify", "Verifying transcoded output", cb)
		if err != nil {
			p.CopyFile(quarantineDest, origPath)
			return "", fmt.Errorf("step 6 failed (read transcoded output): %w", err)
		}
		placedByRename := true
		if err := os.Rename(scratchPath, origPath); err != nil {
			placedByRename = false
			if cerr := p.CopyFileWithProgress(scratchPath, origPath, "destination_copy", "Transferring to destination", cb); cerr != nil {
				p.CopyFile(quarantineDest, origPath)
				return "", fmt.Errorf("step 6 failed (place new): %w (rename: %v)", cerr, err)
			}
		}

		// Step 7: VERIFY NEW — the placed file must be byte-identical to the
		// transcoded output.
		if err := verifyFileHashWithProgress(origPath, scratchHash, "destination_verify", "Verifying file integrity", cb); err != nil {
			os.Remove(origPath)
			p.CopyFile(quarantineDest, origPath)
			return "", fmt.Errorf("step 7 failed (verify new): %w", err)
		}

		// Step 8: CLEANUP — remove scratch file left over if copied.
		if !placedByRename {
			os.Remove(scratchPath)
		}

		// Step 9: Post-transcode original file lifecycle in replace mode.
		if actionOnSuccess == "delete" {
			os.Remove(quarantineDest)
		}
		return origPath, nil
	}

	// For non-replace strategies ("alongside", "alternate"):
	// Step 3: PLACE NEW at destPath.
	scratchHash, err := hashFileWithProgress(scratchPath, "scratch_verify", "Verifying transcoded output", cb)
	if err != nil {
		return "", fmt.Errorf("read transcoded output failed: %w", err)
	}
	placedByRename := true
	if err := os.Rename(scratchPath, destPath); err != nil {
		placedByRename = false
		if cerr := p.CopyFileWithProgress(scratchPath, destPath, "destination_copy", "Transferring to destination", cb); cerr != nil {
			_ = os.Remove(destPath)
			return "", fmt.Errorf("place new failed: %w (rename: %v)", cerr, err)
		}
	}

	// Step 4: VERIFY NEW at destPath.
	if err := verifyFileHashWithProgress(destPath, scratchHash, "destination_verify", "Verifying file integrity", cb); err != nil {
		os.Remove(destPath)
		return "", fmt.Errorf("verify placed output failed: %w", err)
	}
	if !placedByRename {
		os.Remove(scratchPath)
	}

	// Step 5: ACTION ON SUCCESS for original source file.
	switch actionOnSuccess {
	case "delete":
		if err := os.Remove(origPath); err != nil {
			return destPath, fmt.Errorf("action on success delete failed: %w", err)
		}
	case "trash", "archive":
		quarantineDest := paths.Quarantine()
		origHash, _, err := copyAndHashWithProgress(origPath, quarantineDest, "quarantine_backup", "Backing up original", cb)
		if err != nil {
			os.Remove(quarantineDest)
			return destPath, fmt.Errorf("action on success quarantine copy failed: %w", err)
		}
		if err := verifyFileHashWithProgress(quarantineDest, origHash, "quarantine_verify", "Verifying backup integrity", cb); err != nil {
			os.Remove(quarantineDest)
			return destPath, fmt.Errorf("action on success verify quarantine copy failed: %w", err)
		}
		if err := os.Remove(origPath); err != nil {
			return destPath, fmt.Errorf("action on success delete original failed: %w", err)
		}
	case "keep", "retain":
		// Retain original source in place.
	default:
		return destPath, fmt.Errorf("unsupported action on success: %q", actionOnSuccess)
	}

	return destPath, nil
}

// ReplaceSequence orchestrates steps 3 to 8 of ADR-004.
func (p *Pipeline) ReplaceSequence(paths jobpaths.JobPaths) error {
	_, err := p.ApplyPlacementAndDisposition(paths, "replace", "archive", "")
	return err
}
