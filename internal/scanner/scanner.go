package scanner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DefaultScanChannelCapacity bounds the channel buffer for streaming discovered
// file paths to adaptive concurrency workers without buffering entire libraries in memory.
const DefaultScanChannelCapacity = 256

// SupportedMediaExtensions is a list of file extensions the agent will process
var SupportedMediaExtensions = map[string]bool{
	".mkv":  true,
	".mp4":  true,
	".avi":  true,
	".mov":  true,
	".m4v":  true,
	".webm": true,
}

// ScannedFile holds metadata collected from scanning and probing a media file.
type ScannedFile struct {
	FilePath     string  `json:"file_path"`
	RelativePath string  `json:"relative_path,omitempty"`
	Filename     string  `json:"filename"`
	Codec        string  `json:"codec"`
	Duration     float64 `json:"duration"`
	SizeBytes    int64   `json:"size_bytes"`
	Hash         string  `json:"hash"`
	Error        string  `json:"error,omitempty"`
}

// NormalizeRelativePath strips root from filePath and normalizes directory separators
// into a forward-slash POSIX relative path across Linux, macOS, and Windows.
// It rejects paths attempting directory traversal outside the root directory.
func NormalizeRelativePath(root, filePath string) (string, error) {
	if root == "" {
		return "", errors.New("empty root path")
	}
	if filePath == "" {
		return "", errors.New("empty file path")
	}

	cleanRoot := path.Clean(strings.ReplaceAll(root, "\\", "/"))
	cleanPath := path.Clean(strings.ReplaceAll(filePath, "\\", "/"))

	if cleanRoot == cleanPath {
		return "", errors.New("file path is the root directory itself")
	}

	prefix := cleanRoot
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	var rel string
	if strings.HasPrefix(cleanPath, prefix) {
		rel = cleanPath[len(prefix):]
	} else if len(cleanRoot) > 1 && cleanRoot[1] == ':' && len(cleanPath) > 1 && cleanPath[1] == ':' {
		if strings.HasPrefix(strings.ToLower(cleanPath), strings.ToLower(prefix)) {
			rel = cleanPath[len(prefix):]
		}
	}

	if rel == "" {
		if r, err := filepath.Rel(root, filePath); err == nil {
			rSlash := filepath.ToSlash(r)
			if !strings.HasPrefix(rSlash, "../") && rSlash != ".." && rSlash != "." {
				rel = rSlash
			}
		}
	}

	if rel == "" || strings.HasPrefix(rel, "../") || rel == ".." || rel == "." || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("path %q is outside root %q", filePath, root)
	}

	return rel, nil
}


// Walk finds all media files in a directory and its subdirectories.
func Walk(root string) ([]string, error) {
	return WalkWithProgress(root, nil)
}

// WalkWithProgress finds all media files in a directory and its subdirectories,
// invoking onFound for each matching file discovered.
func WalkWithProgress(root string, onFound func(path string, totalFound int)) ([]string, error) {
	var files []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			log.Printf("scanner: skipping inaccessible path %s: %v", path, err)
			return nil
		}

		if !d.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if SupportedMediaExtensions[ext] {
				files = append(files, path)
				if onFound != nil {
					onFound(path, len(files))
				}
			}
		}
		return nil
	})

	return files, err
}

// WalkStream walks root and streams matching media file paths into out.
// If ctx is cancelled, walking stops early and returns ctx.Err().
func WalkStream(ctx context.Context, root string, out chan<- string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if path == root {
				return err
			}
			log.Printf("scanner: skipping inaccessible path %s: %v", path, err)
			return nil
		}

		if !d.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if SupportedMediaExtensions[ext] {
				if ctx != nil {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case out <- path:
					}
				} else {
					out <- path
				}
			}
		}
		return nil
	})
}

// CountMediaFiles counts matching media files under root without allocating path slices in heap memory.
func CountMediaFiles(ctx context.Context, root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if path == root {
				return err
			}
			log.Printf("scanner: skipping inaccessible path %s: %v", path, err)
			return nil
		}

		if !d.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if SupportedMediaExtensions[ext] {
				count++
			}
		}
		return nil
	})
	return count, err
}

// ScanMedia scans a directory, probes each media file with ffprobe using dynamic adaptive concurrency, and generates fingerprints.
func ScanMedia(root string, ffprobePath string) ([]ScannedFile, error) {
	return ScanMediaWithProgress(root, ffprobePath, nil)
}

// ScanMediaWithProgress scans a directory with real-time adaptive concurrency and calls cb after each file is probed.
// Discovered paths are streamed to the prober via a bounded channel rather than accumulating in heap memory.
func ScanMediaWithProgress(root string, ffprobePath string, cb ProbeCallback) ([]ScannedFile, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	totalCount, err := CountMediaFiles(ctx, root)
	if err != nil {
		return nil, err
	}
	if totalCount == 0 {
		return nil, nil
	}

	pathsChan := make(chan string, DefaultScanChannelCapacity)
	errChan := make(chan error, 1)

	go func() {
		defer close(pathsChan)
		if err := WalkStream(ctx, root, pathsChan); err != nil {
			errChan <- err
		}
	}()

	results, err := AdaptiveScanStream(pathsChan, ffprobePath, cb, totalCount)
	select {
	case walkErr := <-errChan:
		if walkErr != nil {
			return nil, walkErr
		}
	default:
	}
	if err == nil {
		for i := range results {
			if rel, relErr := NormalizeRelativePath(root, results[i].FilePath); relErr == nil {
				results[i].RelativePath = rel
			}
		}
	}
	return results, err
}
