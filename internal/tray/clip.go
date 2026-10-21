package tray

// Display text bounds for the compact status window. The GUI renders these
// values into native labels whose intrinsic size feeds the window's minimum
// track size (walk derives WM_GETMINMAXINFO from the layout content and never
// enforces a maximum), so an unbounded string — e.g. a job error embedding
// ffmpeg stderr — can balloon the window to screen size. Capping at the
// display layer keeps any message safe regardless of its origin.
const (
	// MaxErrorLineLen caps the red error line and the last-job message.
	MaxErrorLineLen = 160
	// MaxLogLineLen caps each rolling-log line.
	MaxLogLineLen = 300
)

// ClipForDisplay truncates s to at most max runes for one-line display,
// marking the cut with an ellipsis.
func ClipForDisplay(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
