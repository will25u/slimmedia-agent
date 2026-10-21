package ffmpeg

import (
	"fmt"
	"strings"
)

// disallowedProtocolPrefixes are ffmpeg pseudo-protocols that must never appear
// as the scheme of a local media path. ffmpeg treats e.g. "concat:a|b" or
// "file:/etc/passwd" as protocol references, which would let a server-pushed
// path fetch or read attacker-chosen resources.
var disallowedProtocolPrefixes = []string{
	"async:", "cache:", "concat:", "crypto:", "data:", "file:",
	"ftp:", "gopher:", "http:", "https:", "md5:", "mmsh:", "mmst:",
	"pipe:", "rtmp:", "rtmps:", "rtsp:", "sftp:", "subfile:",
	"tcp:", "tls:", "udp:",
}

// CheckLocalMediaPath rejects paths that could make ffmpeg/ffprobe interpret
// the argument as anything other than a plain local file: remote URLs, ffmpeg
// protocol references, or strings that look like command-line flags.
func CheckLocalMediaPath(path string) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(path, "-") {
		return fmt.Errorf("path %q looks like a command-line flag", path)
	}
	lower := strings.ToLower(path)
	if strings.Contains(lower, "://") {
		return fmt.Errorf("remote URL paths are not allowed: %q", path)
	}
	// Reject a scheme before the first path separator (e.g. "concat:a|b", "lavfi:testsrc").
	if idx := strings.Index(lower, ":"); idx >= 0 {
		if idx == 0 {
			return fmt.Errorf("invalid path %q: leading colon", path)
		}
		sepIdx := strings.IndexAny(lower, "/\\")
		if sepIdx == -1 || idx < sepIdx {
			// Windows drive letters (e.g. "C:\path" or "c:/path") are valid local paths.
			if idx == 1 && ('a' <= lower[0] && lower[0] <= 'z') {
				rem := lower[2:]
				remSepIdx := strings.IndexAny(rem, "/\\")
				remIdx := strings.Index(rem, ":")
				if remIdx != -1 && (remSepIdx == -1 || remIdx < remSepIdx) {
					prefix := rem[:remIdx+1]
					return fmt.Errorf("ffmpeg protocol prefix %q is not allowed in path %q", prefix, path)
				}
			} else {
				prefix := lower[:idx+1]
				return fmt.Errorf("ffmpeg protocol prefix %q is not allowed in path %q", prefix, path)
			}
		}
	}
	return nil
}
