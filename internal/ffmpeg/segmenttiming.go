package ffmpeg

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SegmentTiming records one split chunk's exact position in the source
// timeline as reported by the segment muxer's segment_list CSV. Start and End
// are source-relative seconds anchored on keyframe boundaries; Duration is
// the exact content length the concat demuxer must use for the chunk's
// timeline advance, independent of the chunk container's padded duration.
type SegmentTiming struct {
	// File is the chunk path as recorded by the segment muxer (the expanded
	// output pattern, e.g. /job/segments/split-000.mkv).
	File  string
	Start float64
	End   float64
}

// Duration is the chunk's exact source-timeline length (End - Start).
func (s SegmentTiming) Duration() float64 { return s.End - s.Start }

// ParseSegmentTimingManifest reads the CSV the split phase records via
// -segment_list <path> -segment_list_type csv. Each row is
// "file,start,end" with times in seconds. Malformed rows, non-numeric times,
// and end-before-start entries are errors: the manifest is the contract for
// seam-exact concat durations and keyframe-aware resume, so a corrupt or
// truncated file must never be silently interpreted.
func ParseSegmentTimingManifest(path string) ([]SegmentTiming, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open segment timing manifest: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse segment timing manifest: %w", err)
	}

	var timings []SegmentTiming
	for ln, rec := range records {
		if len(rec) == 0 {
			continue
		}
		if len(rec) != 3 {
			return nil, fmt.Errorf("parse segment timing manifest: line %d has %d fields, want 3 (file,start,end)", ln+1, len(rec))
		}
		start, err := strconv.ParseFloat(strings.TrimSpace(rec[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("parse segment timing manifest: line %d: invalid start time %q: %w", ln+1, rec[1], err)
		}
		end, err := strconv.ParseFloat(strings.TrimSpace(rec[2]), 64)
		if err != nil {
			return nil, fmt.Errorf("parse segment timing manifest: line %d: invalid end time %q: %w", ln+1, rec[2], err)
		}
		if start < 0 || end < start {
			return nil, fmt.Errorf("parse segment timing manifest: line %d has start %.6f, end %.6f (want 0 <= start <= end)", ln+1, start, end)
		}
		timings = append(timings, SegmentTiming{
			File:  strings.TrimSpace(rec[0]),
			Start: start,
			End:   end,
		})
	}
	if len(timings) == 0 {
		return nil, fmt.Errorf("parse segment timing manifest: no entries")
	}
	for i := 1; i < len(timings); i++ {
		if timings[i].Start < timings[i-1].Start {
			return nil, fmt.Errorf("parse segment timing manifest: chunk %d starts at %.6f before chunk %d start %.6f (starts must be non-decreasing)", i, timings[i].Start, i-1, timings[i-1].Start)
		}
		// The muxer records each chunk's end as its last packet's end time,
		// which can precede the next chunk's start on VFR sources: a dropped-
		// frame hole right before the keyframe boundary leaves no packet to
		// carry the boundary time. Such a forward gap is benign — the concat
		// derives each non-last chunk's duration from the next chunk's start,
		// so ranges still sum to the source span — but an overlap means the
		// manifest is corrupt and the durations cannot be trusted. The 1ms
		// allowance absorbs decimal rounding at the boundary.
		if d := timings[i].Start - timings[i-1].End; d < -0.001 {
			return nil, fmt.Errorf("parse segment timing manifest: chunk %d start %.6f overlaps chunk %d end %.6f (ranges must not overlap)", i, timings[i].Start, i-1, timings[i-1].End)
		}
	}
	return timings, nil
}
