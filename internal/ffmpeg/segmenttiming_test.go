package ffmpeg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseSegmentTimingManifest(t *testing.T) {
	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "segments.csv")
	content := "/job/segments/split-000.mkv,0.000000,2.021000\n" +
		"/job/segments/split-001.mkv,2.021000,4.021000\n" +
		"/job/segments/split-002.mkv,4.021000,5.021000\n"
	if err := os.WriteFile(manifest, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	timings, err := ParseSegmentTimingManifest(manifest)
	if err != nil {
		t.Fatalf("ParseSegmentTimingManifest: %v", err)
	}
	if len(timings) != 3 {
		t.Fatalf("got %d entries, want 3", len(timings))
	}
	want := []SegmentTiming{
		{File: "/job/segments/split-000.mkv", Start: 0.0, End: 2.021},
		{File: "/job/segments/split-001.mkv", Start: 2.021, End: 4.021},
		{File: "/job/segments/split-002.mkv", Start: 4.021, End: 5.021},
	}
	for i, w := range want {
		if timings[i] != w {
			t.Errorf("entry %d = %+v, want %+v", i, timings[i], w)
		}
		if d := timings[i].Duration(); d != w.End-w.Start {
			t.Errorf("entry %d Duration() = %f, want %f", i, d, w.End-w.Start)
		}
	}
}

func TestParseSegmentTimingManifestErrors(t *testing.T) {
	cases := map[string]string{
		"wrong field count": "split-000.mkv,0.000000\n",
		"non-numeric start": "split-000.mkv,abc,2.000000\n",
		"non-numeric end":   "split-000.mkv,0.000000,xyz\n",
		"end before start":  "split-000.mkv,2.000000,1.000000\n",
		"negative start":    "split-000.mkv,-1.000000,2.000000\n",
		"no entries":        "",
		"decreasing starts":      "a.mkv,2.000000,3.000000\nb.mkv,1.000000,2.500000\n",
		"overlap between chunks": "a.mkv,0.000000,2.000000\nb.mkv,1.500000,4.000000\n",
		"blank lines only":       "\n\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			manifest := filepath.Join(tmp, "segments.csv")
			if err := os.WriteFile(manifest, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := ParseSegmentTimingManifest(manifest); err == nil {
				t.Fatalf("expected error for %q, got nil", content)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := ParseSegmentTimingManifest(filepath.Join(t.TempDir(), "nope.csv")); err == nil {
			t.Fatal("expected error for missing manifest, got nil")
		}
	})
}

// TestParseSegmentTimingManifestToleratesVFRBoundaryGap: the segment muxer
// records a chunk's end as its last packet's end time, so on a VFR source
// with a dropped-frame hole right before the keyframe boundary the next
// chunk's start lands after the recorded end (here 1.96 -> 2.00). That gap is
// the muxer's end-semantics, not corruption: the concat derives each non-last
// chunk's duration from the next chunk's start, so parsing must accept it
// while still rejecting true overlaps.
func TestParseSegmentTimingManifestToleratesVFRBoundaryGap(t *testing.T) {
	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "segments.csv")
	content := "/job/segments/split-000.mkv,0.000000,1.960000\n" +
		"/job/segments/split-001.mkv,2.000000,3.960000\n" +
		"/job/segments/split-002.mkv,4.000000,4.960000\n"
	if err := os.WriteFile(manifest, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	timings, err := ParseSegmentTimingManifest(manifest)
	if err != nil {
		t.Fatalf("ParseSegmentTimingManifest: %v", err)
	}
	if len(timings) != 3 {
		t.Fatalf("got %d entries, want 3", len(timings))
	}
	// The seam-advance each non-last chunk must receive: next start minus
	// own start (2.0 and 1.96 here), not the recorded end minus start.
	for i, want := range []float64{2.0, 2.0} {
		if got := timings[i+1].Start - timings[i].Start; got != want {
			t.Errorf("chunk %d seam advance = %f, want %f", i, got, want)
		}
	}
}

func TestParseSegmentTimingManifestToleratesQuotedFilename(t *testing.T) {
	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "segments.csv")
	// FFmpeg's csv writer quotes fields containing commas; the parser must
	// accept both plain and quoted filenames.
	content := "\"/job/segments/split,000.mkv\",0.000000,2.000000\n"
	if err := os.WriteFile(manifest, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	timings, err := ParseSegmentTimingManifest(manifest)
	if err != nil {
		t.Fatalf("ParseSegmentTimingManifest: %v", err)
	}
	if len(timings) != 1 || !strings.Contains(timings[0].File, "split,000.mkv") {
		t.Fatalf("quoted filename not parsed: %+v", timings)
	}
}

// probePacketPTSTimes returns the sorted presentation timestamps of every
// packet in the file's streams matching the ffprobe stream selector (e.g.
// "v:0", "a:0", "s"), for timestamp-placement assertions in integration
// tests.
func probePacketPTSTimes(ffprobePath, filePath, streamSelector string) ([]float64, error) {
	if err := CheckLocalMediaPath(filePath); err != nil {
		return nil, fmt.Errorf("invalid probe path: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-select_streams", streamSelector,
		"-show_entries", "packet=pts_time",
		"-of", "csv=p=0",
		filePath,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("packet pts probe failed: %w", err)
	}
	var pts []float64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
		if err != nil {
			return nil, fmt.Errorf("packet pts probe returned unparsable value %q: %w", line, err)
		}
		pts = append(pts, v)
	}
	sort.Float64s(pts)
	return pts, nil
}

// probeVideoPacketPTSTimes returns the sorted presentation timestamps of
// every video packet in the file, for seam-placement assertions in
// integration tests.
func probeVideoPacketPTSTimes(ffprobePath, filePath string) ([]float64, error) {
	return probePacketPTSTimes(ffprobePath, filePath, "v:0")
}
