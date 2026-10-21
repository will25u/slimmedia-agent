package tray

import (
	"strings"
	"testing"
)

func TestClipForDisplay(t *testing.T) {
	if got := ClipForDisplay("", 10); got != "" {
		t.Errorf("empty = %q", got)
	}
	short := "source demux failed"
	if got := ClipForDisplay(short, MaxErrorLineLen); got != short {
		t.Errorf("short string must pass through, got %q", got)
	}
	long := strings.Repeat("x", 4000) // a wire-capped job error embedding ffmpeg stderr
	got := ClipForDisplay(long, MaxErrorLineLen)
	if runes := len([]rune(got)); runes != MaxErrorLineLen {
		t.Errorf("clipped length = %d runes, want %d", runes, MaxErrorLineLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clipped string must end with an ellipsis: %q", got)
	}
	if got := ClipForDisplay("ab", 1); got != "…" {
		t.Errorf("max=1 = %q, want ellipsis", got)
	}
	if got := ClipForDisplay("ab", 0); got != "" {
		t.Errorf("max=0 = %q, want empty", got)
	}
}
