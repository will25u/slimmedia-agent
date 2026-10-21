package licenses

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintNotice(t *testing.T) {
	var buf bytes.Buffer
	PrintNotice(&buf, false)
	out := buf.String()

	if !strings.Contains(out, "License version 3.0 (GPLv3)") {
		t.Errorf("expected GPLv3 notice, got %s", out)
	}
	if !strings.Contains(out, "https://github.com/BtbN/FFmpeg-Builds") {
		t.Errorf("expected BtbN source offer link, got %s", out)
	}
	if !strings.Contains(out, "https://www.gyan.dev/ffmpeg/builds/") {
		t.Errorf("expected gyan.dev source offer link, got %s", out)
	}
	if strings.Contains(out, "TERMS AND CONDITIONS") {
		t.Errorf("expected short notice not to contain full terms")
	}

	buf.Reset()
	PrintNotice(&buf, true)
	fullOut := buf.String()
	if !strings.Contains(fullOut, "TERMS AND CONDITIONS") {
		t.Errorf("expected full notice to contain terms and conditions")
	}
}
