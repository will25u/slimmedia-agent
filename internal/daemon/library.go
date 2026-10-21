package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/scanner"
	"github.com/will25u/slimmedia-agent/internal/transcoder"
)

// isPathConfined reports whether target is confined strictly within root.
// It resolves symlinks for both paths and verifies target is a descendant of root.
// When target does not yet exist on disk, it evaluates parent directory symlinks
// and fails closed if parent directories cannot be resolved or escape root.
func isPathConfined(root, target string) (bool, error) {
	if root == "" || target == "" {
		return false, nil
	}
	canonicalRoot, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(canonicalRoot)
	if err != nil {
		return false, err
	}
	canonicalRoot = filepath.Clean(resolvedRoot)

	canonicalTarget, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	canonicalTarget = filepath.Clean(canonicalTarget)

	if resolved, err := filepath.EvalSymlinks(canonicalTarget); err == nil {
		canonicalTarget = filepath.Clean(resolved)
	} else {
		// If target exists (e.g. as a broken symlink), fail closed.
		if _, lerr := os.Lstat(canonicalTarget); lerr == nil {
			return false, err
		}
		// When target does not yet exist on disk, evaluate parent directory symlinks.
		parent := filepath.Dir(canonicalTarget)
		resolvedParent, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return false, err
		}
		resolvedParent = filepath.Clean(resolvedParent)
		relParent, err := filepath.Rel(canonicalRoot, resolvedParent)
		if err != nil {
			return false, err
		}
		if relParent != "." && (!filepath.IsLocal(relParent) || relParent == ".." || strings.HasPrefix(relParent, ".."+string(filepath.Separator))) {
			return false, nil
		}
		canonicalTarget = filepath.Join(resolvedParent, filepath.Base(canonicalTarget))
	}

	rel, err := filepath.Rel(canonicalRoot, canonicalTarget)
	if err != nil {
		return false, err
	}
	if !filepath.IsLocal(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

// dbLibrarySet adapts the agent database to the transcoder.ScannedLibrary
// allowlist used for job path confinement.
type dbLibrarySet struct {
	db       *sql.DB
	mediaDir string
}

func (s dbLibrarySet) Contains(canonicalPath string) (bool, error) {
	if s.mediaDir != "" {
		if confined, err := isPathConfined(s.mediaDir, canonicalPath); err == nil && confined {
			return true, nil
		}
	}
	if s.db != nil {
		return db.New(s.db).HasScannedFile(context.Background(), canonicalPath)
	}
	return false, nil
}

// scannedLibraryConfinement adapts validateJobInputPath into the
// JobSupervisor's InputValidator seam.
type scannedLibraryConfinement struct {
	db       *sql.DB
	mediaDir string
}

func (c scannedLibraryConfinement) ValidateJobInputPath(inputPath string) (string, error) {
	return validateJobInputPath(c.db, inputPath, c.mediaDir)
}

// validateJobInputPath resolves a server-supplied job input path to its
// canonical local form and verifies the local scanner discovered it or that
// it is confined within the media directory. The returned path is safe to hand
// to the transcode pipeline.
func validateJobInputPath(agentDB *sql.DB, inputPath string, mediaDirs ...string) (string, error) {
	canonical, err := transcoder.CanonicalMediaPath(inputPath)
	if err != nil {
		return "", err
	}
	for _, dir := range mediaDirs {
		if dir != "" {
			if confined, err := isPathConfined(dir, canonical); err == nil && confined {
				return canonical, nil
			}
		}
	}
	if agentDB != nil {
		exists, err := db.New(agentDB).HasScannedFile(context.Background(), canonical)
		if err == nil && exists {
			return canonical, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the scanned media library", canonical)
}

// recordScannedFiles persists the canonical paths discovered by the local
// scanner so later server-pushed jobs can be confined to them.
func recordScannedFiles(agentDB *sql.DB, items []scanner.ScannedFile, mediaDir string) {
	if agentDB == nil || len(items) == 0 {
		return
	}

	tx, err := agentDB.BeginTx(context.Background(), nil)
	if err != nil {
		log.Printf("Failed to begin transaction for recording scanned files: %v", err)
		return
	}
	defer tx.Rollback()

	canonicalMediaDir, err := filepath.Abs(mediaDir)
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(canonicalMediaDir); err == nil {
			canonicalMediaDir = resolved
		}
	}
	canonicalMediaDir = filepath.Clean(canonicalMediaDir)

	stmt, err := tx.PrepareContext(context.Background(),
		"INSERT INTO scanned_files (file_path, scanned_at) VALUES (?, CURRENT_TIMESTAMP) "+
			"ON CONFLICT(file_path) DO UPDATE SET scanned_at = CURRENT_TIMESTAMP")
	if err != nil {
		log.Printf("Failed to prepare statement for scanned files: %v", err)
		return
	}
	defer stmt.Close()

	recorded := 0
	for _, item := range items {
		clean := filepath.Clean(item.FilePath)
		if !filepath.IsAbs(clean) {
			if abs, err := filepath.Abs(clean); err == nil {
				clean = abs
			}
		}

		canonical := clean
		if fi, err := os.Lstat(clean); err == nil && (fi.Mode()&os.ModeSymlink != 0) {
			if resolved, err := filepath.EvalSymlinks(clean); err == nil {
				canonical = resolved
			}
		}

		rel, err := filepath.Rel(canonicalMediaDir, canonical)
		if err != nil || !filepath.IsLocal(rel) {
			log.Printf("Skipping scanned path %q: resolves outside media directory", item.FilePath)
			continue
		}

		if _, err := stmt.ExecContext(context.Background(), canonical); err != nil {
			log.Printf("Failed to record scanned path %q: %v", canonical, err)
			continue
		}
		recorded++
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction for scanned files: %v", err)
		return
	}
	log.Printf("Recorded %d scanned file path(s) in the local library registry", recorded)
}
