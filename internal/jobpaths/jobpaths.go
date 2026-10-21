// Package jobpaths owns the per-job filesystem layout: the namespaced
// scratch file, the segment directory and chunk names inside it, the concat
// manifest, the job journal payload, and the quarantine destination. Every
// consumer (orchestrator, ffmpeg runner, crash recovery, daemon cancel path)
// derives these paths from a JobPaths value instead of re-implementing the
// convention.
package jobpaths

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	// segmentSuffix marks the per-job segment directory that lives next to
	// the scratch output file.
	segmentSuffix = ".segments"
	// splitChunkFormat names the stream-copy split output from Phase 1.
	splitChunkFormat = "split-%03d.mkv"
	// splitChunkPattern is the printf-style pattern passed to the segment muxer.
	splitChunkGlob = "split-%03d.mkv"
	// encodedChunkFormat names the transcoded output from Phase 2.
	encodedChunkFormat = "chunk-%03d.mkv"
	// chunkFormat is the legacy name kept for crash-recovery compatibility.
	// New code should use encodedChunkFormat.
	chunkFormat = "chunk-%03d.mkv"
	// concatName is the ffmpeg concat manifest written into the segment dir.
	concatName = "concat.txt"
	// journalSuffix marks the job payload persisted next to the scratch file
	// so a crash can be resumed. It deliberately does not live in the segment
	// directory: single-pass jobs (hardware encoders) never create that
	// directory but still need crash recovery.
	journalSuffix = ".job.json"
	// demuxName is the local scratch container holding every audio/subtitle
	// track and the chapter markers extracted from the source in the upfront
	// stream-copy pass. Lives in the segment dir so per-job namespacing and
	// cleanup come for free.
	demuxName = "demux.mkv"
	// chaptersName is the local ffmetadata chapter sidecar for sources that
	// carry chapter markers but no audio/subtitle streams (the demux
	// container cannot be built from zero streams). Final assembly maps
	// chapters from it instead of re-reading the source.
	chaptersName = "chapters.ffmeta"
	// segmentTimingName is the segment-muxer timing manifest (segment_list
	// CSV): one row per split chunk with its exact start and end in the
	// source timeline. It drives the concat manifest's duration directives
	// and keyframe-aware resume, so chunks that start at real (not nominal)
	// boundaries carry no cumulative seam padding.
	segmentTimingName = "segments.csv"
)

// NamespacedFileName maps a canonical media path to a unique scratch or
// quarantine file name: sha256(canonical path)[:16] + "-" + base. Two files
// that share a basename in different directories get distinct slots, so one
// can never overwrite the other's only backup.
func NamespacedFileName(canonicalPath string) string {
	sum := sha256.Sum256([]byte(canonicalPath))
	return fmt.Sprintf("%x-%s", sum[:8], filepath.Base(canonicalPath))
}

// JobPaths is the complete set of derived filesystem paths for a single
// transcode job. Construct it once per job with New (or FromOutput for
// runner-side use) and pass it to every stage that needs a path.
type JobPaths struct {
	source     string // canonical media path (empty for FromOutput)
	scratch    string // scratch working copy of the transcoded output
	segmentDir string
	journal    string // persisted job payload for crash recovery
	quarantine string
}

// New derives every path for the job that transcodes canonicalPath. The
// canonical path is the resolved media file both the scratch and quarantine
// names are namespaced from.
func New(scratchDir, quarantineDir, canonicalPath string) JobPaths {
	scratch := filepath.Join(scratchDir, NamespacedFileName(canonicalPath))
	stem := strings.TrimSuffix(filepath.Base(scratch), filepath.Ext(scratch))
	return JobPaths{
		source:     canonicalPath,
		scratch:    scratch,
		segmentDir: filepath.Join(scratchDir, stem+segmentSuffix),
		journal:    filepath.Join(scratchDir, stem+journalSuffix),
		quarantine: filepath.Join(quarantineDir, NamespacedFileName(canonicalPath)),
	}
}

// FromOutput derives the runner-side paths from the scratch output file
// alone. Used when the caller (a test, a legacy runner invocation) knows the
// output path but not the quarantine layout; Quarantine() is empty in that
// case.
func FromOutput(outputFile string) JobPaths {
	stem := strings.TrimSuffix(filepath.Base(outputFile), filepath.Ext(outputFile))
	return JobPaths{
		scratch:    outputFile,
		segmentDir: filepath.Join(filepath.Dir(outputFile), stem+segmentSuffix),
		journal:    filepath.Join(filepath.Dir(outputFile), stem+journalSuffix),
	}
}

// Source returns the canonical media path the job operates on.
func (p JobPaths) Source() string { return p.source }

// Scratch returns the scratch file the transcoded output is assembled in
// before it replaces the original.
func (p JobPaths) Scratch() string { return p.scratch }

// SegmentDir returns the per-job directory holding encode chunks, the concat
// manifest, the segment timing manifest, and the job journal payload.
func (p JobPaths) SegmentDir() string { return p.segmentDir }

// Quarantine returns the quarantine destination for the original file.
func (p JobPaths) Quarantine() string { return p.quarantine }

// Chunk returns the path of the encoded chunk at index (legacy / crash-recovery name).
func (p JobPaths) Chunk(index int) string {
	return filepath.Join(p.segmentDir, fmt.Sprintf(chunkFormat, index))
}

// SplitChunk returns the path of the stream-copy split chunk produced by
// Phase 1 (the segment muxer pass).
func (p JobPaths) SplitChunk(index int) string {
	return filepath.Join(p.segmentDir, fmt.Sprintf(splitChunkFormat, index))
}

// EncodedChunk returns the path of the transcoded output for Phase 2 at index.
func (p JobPaths) EncodedChunk(index int) string {
	return filepath.Join(p.segmentDir, fmt.Sprintf(encodedChunkFormat, index))
}

// SplitChunkPattern returns the printf-style pattern string passed to the
// FFmpeg segment muxer so it names its outputs split-000.mkv, split-001.mkv, …
func (p JobPaths) SplitChunkPattern() string {
	return filepath.Join(p.segmentDir, splitChunkGlob)
}

// ConcatFile returns the ffmpeg concat manifest path.
func (p JobPaths) ConcatFile() string {
	return filepath.Join(p.segmentDir, concatName)
}

// JournalFile returns the path of the persisted job payload used for crash
// recovery. It lives next to the scratch file (not in the segment directory)
// so single-pass jobs that never create a segment directory can still be
// recovered.
func (p JobPaths) JournalFile() string { return p.journal }

// LegacyJournalFile returns the pre-single-pass journal location inside the
// segment directory ("job.json"). Only crash recovery consults it, as a
// fallback for jobs that were in flight when the journal moved; new writes
// always go to JournalFile.
func (p JobPaths) LegacyJournalFile() string {
	return filepath.Join(p.segmentDir, "job.json")
}

// DemuxContainer returns the local scratch container that holds every
// audio/subtitle track and the chapter markers extracted from the source by
// the upfront stream-copy pass. Downstream phases consume this instead of
// re-reading the (possibly remote) source.
func (p JobPaths) DemuxContainer() string {
	return filepath.Join(p.segmentDir, demuxName)
}

// ChapterFile returns the local ffmetadata sidecar holding the source's
// chapter markers when the source has no audio/subtitle streams (and so no
// demux container). Final assembly maps chapters from this file, keeping the
// stitch a local-only operation.
func (p JobPaths) ChapterFile() string {
	return filepath.Join(p.segmentDir, chaptersName)
}

// SegmentTimingManifest returns the segment-muxer timing manifest (CSV) the
// split phase writes alongside the chunks: one row per chunk recording its
// exact start and end in the source timeline. The concat manifest derives
// per-chunk duration directives from it, and resume validates the recorded
// chunk list against it, so sources with no keyframe inside a segment window
// resume without re-splitting.
func (p JobPaths) SegmentTimingManifest() string {
	return filepath.Join(p.segmentDir, segmentTimingName)
}

var allowedContainers = map[string]bool{
	"mkv":  true,
	"mp4":  true,
	"m4v":  true,
	"mov":  true,
	"webm": true,
}

// DestinationPath calculates the final target path for the transcoded output based
// on the given output file strategy ("replace", "alongside", "alternate") and optional
// container extension.
func (p JobPaths) DestinationPath(strategy, container string) (string, error) {
	var clean string
	if container != "" {
		if strings.Contains(container, "..") || strings.Contains(container, "/") || strings.Contains(container, "\\") {
			return "", fmt.Errorf("container extension %q contains forbidden path traversal characters", container)
		}
		clean = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(container), "."))
		if !allowedContainers[clean] {
			return "", fmt.Errorf("unsupported container extension %q: must be one of mkv, mp4, m4v, mov, webm", container)
		}
	}

	if p.source == "" {
		return p.scratch, nil
	}
	ext := filepath.Ext(p.source)
	if clean != "" {
		ext = "." + clean
	}
	stem := strings.TrimSuffix(filepath.Base(p.source), filepath.Ext(p.source))

	switch strategy {
	case "alongside":
		return filepath.Join(filepath.Dir(p.source), stem+".transcoded"+ext), nil
	case "alternate":
		altDir := filepath.Join(filepath.Dir(p.quarantine), "transcoded")
		return filepath.Join(altDir, stem+ext), nil
	case "replace", "":
		return p.source, nil
	default:
		return "", fmt.Errorf("unrecognized output file strategy: %q", strategy)
	}
}
