package scanner

import (
	"fmt"
	"io"
	"os"

	"github.com/cespare/xxhash/v2"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
)

const MB = 1024 * 1024

// Fingerprint calculates the xxHash of the first 1MB, last 1MB, and file size.
// It opens the file with safe path pinning (OpenNoFollow) to prevent symlink traversal.
func Fingerprint(filePath string) (string, error) {
	file, err := ffmpeg.OpenNoFollow(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file for hashing: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}

	size := info.Size()
	hasher := xxhash.New()

	// Read first 1MB
	if err := readChunkAndHash(file, hasher); err != nil {
		return "", fmt.Errorf("failed to read first MB: %w", err)
	}

	// If file is larger than 1MB, seek to last 1MB and read
	if size > MB {
		lastMBSize := size - MB
		var offset int64
		if lastMBSize > MB {
			offset = size - MB
		} else {
			// File is between 1MB and 2MB, so last chunk overlaps or starts immediately after
			offset = MB
		}

		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return "", fmt.Errorf("failed to seek to last MB: %w", err)
		}

		if err := readChunkAndHash(file, hasher); err != nil {
			return "", fmt.Errorf("failed to read last MB: %w", err)
		}
	}

	// Add file size to hash
	sizeStr := fmt.Sprintf("size:%d", size)
	hasher.Write([]byte(sizeStr))

	return fmt.Sprintf("%016x", hasher.Sum64()), nil
}

func readChunkAndHash(file *os.File, hasher *xxhash.Digest) error {
	buf := make([]byte, MB)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	hasher.Write(buf[:n])
	return nil
}
