package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/licenses"
)

func TestLicenseNoticeOutput(t *testing.T) {
	var buf bytes.Buffer
	licenses.PrintNotice(&buf, false)
	out := buf.String()

	if !strings.Contains(out, "FFmpeg") {
		t.Fatalf("expected FFmpeg in license notice, got %s", out)
	}
	if !strings.Contains(out, "GPLv3") {
		t.Fatalf("expected GPLv3 in license notice, got %s", out)
	}
	if !strings.Contains(out, "hello@slimmedia.app") {
		t.Fatalf("expected contact email in license notice, got %s", out)
	}
}
