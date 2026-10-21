package jobpaths

import (
	"path/filepath"
	"testing"
)

func TestNamespacedFileNameDistinctForSameBasename(t *testing.T) {
	nameA := NamespacedFileName("/media/a/movie.mkv")
	nameB := NamespacedFileName("/media/b/movie.mkv")
	if nameA == nameB {
		t.Fatalf("expected distinct slot names for same-basename files, got %q", nameA)
	}
}

func TestNewDerivesAllPaths(t *testing.T) {
	p := New("/scratch", "/quarantine", "/media/movies/movie.mkv")

	wantScratch := filepath.Join("/scratch", NamespacedFileName("/media/movies/movie.mkv"))
	if p.Scratch() != wantScratch {
		t.Errorf("Scratch() = %q, want %q", p.Scratch(), wantScratch)
	}
	if p.Source() != "/media/movies/movie.mkv" {
		t.Errorf("Source() = %q", p.Source())
	}

	stem := NamespacedFileName("/media/movies/movie.mkv")
	stem = stem[:len(stem)-len(".mkv")]
	wantSeg := filepath.Join("/scratch", stem+".segments")
	if p.SegmentDir() != wantSeg {
		t.Errorf("SegmentDir() = %q, want %q", p.SegmentDir(), wantSeg)
	}

	if got, want := p.JournalFile(), filepath.Join("/scratch", stem+".job.json"); got != want {
		t.Errorf("JournalFile() = %q, want %q", got, want)
	}
	if got, want := p.LegacyJournalFile(), filepath.Join(wantSeg, "job.json"); got != want {
		t.Errorf("LegacyJournalFile() = %q, want %q", got, want)
	}

	if got, want := p.JournalFile(), filepath.Join("/scratch", stem+".job.json"); got != want {
		t.Errorf("JournalFile() = %q, want %q", got, want)
	}
	if got, want := p.LegacyJournalFile(), filepath.Join(wantSeg, "job.json"); got != want {
		t.Errorf("LegacyJournalFile() = %q, want %q", got, want)
	}
	if got, want := p.Chunk(0), filepath.Join(wantSeg, "chunk-000.mkv"); got != want {
		t.Errorf("Chunk(0) = %q, want %q", got, want)
	}
	if got, want := p.Chunk(12), filepath.Join(wantSeg, "chunk-012.mkv"); got != want {
		t.Errorf("Chunk(12) = %q, want %q", got, want)
	}
	if got, want := p.ConcatFile(), filepath.Join(wantSeg, "concat.txt"); got != want {
		t.Errorf("ConcatFile() = %q, want %q", got, want)
	}
	if got, want := p.DemuxContainer(), filepath.Join(wantSeg, "demux.mkv"); got != want {
		t.Errorf("DemuxContainer() = %q, want %q", got, want)
	}
	if got, want := p.ChapterFile(), filepath.Join(wantSeg, "chapters.ffmeta"); got != want {
		t.Errorf("ChapterFile() = %q, want %q", got, want)
	}
	if got, want := p.SegmentTimingManifest(), filepath.Join(wantSeg, "segments.csv"); got != want {
		t.Errorf("SegmentTimingManifest() = %q, want %q", got, want)
	}

	wantQuar := filepath.Join("/quarantine", NamespacedFileName("/media/movies/movie.mkv"))
	if p.Quarantine() != wantQuar {
		t.Errorf("Quarantine() = %q, want %q", p.Quarantine(), wantQuar)
	}
}

func TestFromOutputMatchesNewSegmentLayout(t *testing.T) {
	p := New("/scratch", "/quarantine", "/media/movies/movie.mkv")
	f := FromOutput(p.Scratch())

	if f.Scratch() != p.Scratch() {
		t.Errorf("FromOutput Scratch() = %q, want %q", f.Scratch(), p.Scratch())
	}
	if f.SegmentDir() != p.SegmentDir() {
		t.Errorf("FromOutput SegmentDir() = %q, want %q", f.SegmentDir(), p.SegmentDir())
	}
	if f.Chunk(3) != p.Chunk(3) {
		t.Errorf("FromOutput Chunk(3) = %q, want %q", f.Chunk(3), p.Chunk(3))
	}
	if f.ConcatFile() != p.ConcatFile() {
		t.Errorf("FromOutput ConcatFile() = %q, want %q", f.ConcatFile(), p.ConcatFile())
	}
	if f.DemuxContainer() != p.DemuxContainer() {
		t.Errorf("FromOutput DemuxContainer() = %q, want %q", f.DemuxContainer(), p.DemuxContainer())
	}
	if f.ChapterFile() != p.ChapterFile() {
		t.Errorf("FromOutput ChapterFile() = %q, want %q", f.ChapterFile(), p.ChapterFile())
	}
}

func TestDestinationPath(t *testing.T) {
	p := New("/scratch", "/quarantine", "/media/movies/action.mkv")

	checkPath := func(strat, cont, want string) {
		t.Helper()
		got, err := p.DestinationPath(strat, cont)
		if err != nil {
			t.Fatalf("unexpected error for %s / %s: %v", strat, cont, err)
		}
		if filepath.Clean(got) != filepath.Clean(want) {
			t.Errorf("DestinationPath(%q, %q) = %q, want %q", strat, cont, got, want)
		}
	}

	checkPath("replace", "", "/media/movies/action.mkv")
	checkPath("", "", "/media/movies/action.mkv")
	checkPath("alongside", "", "/media/movies/action.transcoded.mkv")
	checkPath("alongside", "mp4", "/media/movies/action.transcoded.mp4")
	checkPath("alongside", "mkv", "/media/movies/action.transcoded.mkv")
	checkPath("alongside", "m4v", "/media/movies/action.transcoded.m4v")
	checkPath("alongside", "mov", "/media/movies/action.transcoded.mov")
	checkPath("alongside", "webm", "/media/movies/action.transcoded.webm")
	checkPath("alternate", "", "/quarantine/transcoded/action.mkv")
	checkPath("alternate", "mp4", "/quarantine/transcoded/action.mp4")

	// Traversal and non-media extension rejection
	rejectCases := []string{
		"../evil",
		"..",
		"../../etc/cron.d/job",
		"/tmp/out.mkv",
		`..\win.mp4`,
		`foo\bar`,
		"exe",
		"sh",
		"txt",
		"avi",
		"flv",
		".evil",
	}

	for _, tc := range rejectCases {
		if _, err := p.DestinationPath("alongside", tc); err == nil {
			t.Errorf("expected error for container %q, got nil", tc)
		}
		if _, err := p.DestinationPath("replace", tc); err == nil {
			t.Errorf("expected error for container %q in replace mode, got nil", tc)
		}
	}

	// Unrecognized strategy rejection: DestinationPath must never return source path
	unrecognizedStrategies := []string{"hacked", "default", "overwrite", "unknown", "../source", "archive", "delete"}
	for _, strat := range unrecognizedStrategies {
		dest, err := p.DestinationPath(strat, "")
		if err == nil {
			t.Errorf("expected error for unrecognized strategy %q, got dest %q", strat, dest)
		}
		if dest == p.Source() {
			t.Errorf("critical security failure: DestinationPath(%q) returned source path %q", strat, dest)
		}
	}
}
