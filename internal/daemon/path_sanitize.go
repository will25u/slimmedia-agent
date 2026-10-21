package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// isShellEscapable reports whether byte c is a character commonly escaped in Unix shells.
func isShellEscapable(c byte) bool {
	switch c {
	case ' ', '\t', '&', '(', ')', '[', ']', '{', '}', '\'', '"', '!', '$', ';', '*', '?', '<', '>', '|', '^', '#', ',', '@', '%', '+', '=', '~', ':':
		return true
	default:
		return false
	}
}

// unescapeShellPath removes literal shell-escape backslashes before special characters on Unix.
func unescapeShellPath(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			next := s[i+1]
			if isShellEscapable(next) {
				buf.WriteByte(next)
				i++
				continue
			}
		}
		buf.WriteByte(s[i])
	}
	return buf.String()
}

// CleanLocalPath sanitizes and normalizes an incoming local filesystem path:
// - Strips leading/trailing whitespace
// - On Linux/Unix, unescapes shell-escaped literal characters (e.g., \  ->  , \& -> &)
// - Normalizes separators using filepath.Clean and runtime conversions so paths never mix / and \
// - Strips redundant trailing slashes (except root "/" or "C:\")
func CleanLocalPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}

	if runtime.GOOS != "windows" {
		p = unescapeShellPath(p)
		p = strings.ReplaceAll(p, "\\", "/")
	} else {
		p = filepath.FromSlash(p)
	}

	p = filepath.Clean(p)

	// Strip redundant trailing slashes
	if len(p) > 1 && strings.HasSuffix(p, string(filepath.Separator)) {
		// Preserve root "/" or drive root "C:\"
		if runtime.GOOS != "windows" || len(p) > 3 || (len(p) == 3 && p[1] != ':') {
			p = strings.TrimRight(p, string(filepath.Separator))
		}
	}

	return p
}

// ValidateLocalLibraryDirectory checks if a path exists, is a directory, and has read permissions.
// Returns (true, "") on success, or (false, reason) with a human-readable failure reason.
func ValidateLocalLibraryDirectory(p string) (bool, string) {
	if p == "" {
		return false, "Directory does not exist"
	}

	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "Directory does not exist"
		}
		if os.IsPermission(err) {
			return false, "Permission denied"
		}
		return false, err.Error()
	}
	if !st.IsDir() {
		return false, "Path is not a directory"
	}

	f, err := os.Open(p)
	if err != nil {
		if os.IsPermission(err) {
			return false, "Permission denied"
		}
		return false, fmt.Sprintf("Permission denied: %v", err)
	}
	defer f.Close()

	_, rErr := f.ReadDir(1)
	if rErr != nil && !errors.Is(rErr, io.EOF) {
		if os.IsPermission(rErr) {
			return false, "Permission denied reading directory"
		}
		return false, fmt.Sprintf("Permission denied reading directory: %v", rErr)
	}

	return true, ""
}
