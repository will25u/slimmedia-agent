package ffmpeg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
)

func TestCheckLocalMediaPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr string
	}{
		{"plain relative", "movie.mkv", ""},
		{"plain absolute", "/media/library/movie.mkv", ""},
		{"spaces and colons after separator", "/media/library/foo: bar.mkv", ""},
		{"empty", "", "empty path"},
		{"leading dash flag", "-something", "command-line flag"},
		{"http URL", "http://169.254.169.254/x", "remote URL"},
		{"https URL", "https://example.com/a.mkv", "remote URL"},
		{"uppercase HTTP", "HTTP://169.254.169.254/x", "remote URL"},
		{"concat protocol", "concat:a|b", "ffmpeg protocol prefix"},
		{"file protocol", "file:/etc/passwd", "ffmpeg protocol prefix"},
		{"pipe protocol", "pipe:0", "ffmpeg protocol prefix"},
		{"data protocol", "data:video/mp4;base64,AAA", "ffmpeg protocol prefix"},
		{"lavfi unusual protocol", "lavfi:testsrc=size=1920x1080", "ffmpeg protocol prefix"},
		{"bluray unusual protocol", "bluray:/dev/sr0", "ffmpeg protocol prefix"},
		{"srt protocol prefix", "srt:streamid=test", "ffmpeg protocol prefix"},
		{"unix socket protocol", "unix:/var/run/media.sock", "ffmpeg protocol prefix"},
		{"arbitrary unknown protocol", "customproto:stream", "ffmpeg protocol prefix"},
		{"leading colon", ":invalid", "leading colon"},
		{"windows drive backslash", `C:\media\library\movie.mkv`, ""},
		{"windows drive forward slash", "c:/media/library/movie.mkv", ""},
		{"windows drive with protocol suffix", "C:concat:a|b", "ffmpeg protocol prefix"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckLocalMediaPath(tc.path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error for %q, got: %v", tc.path, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q for %q, got nil", tc.wantErr, tc.path)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q for %q, got: %v", tc.wantErr, tc.path, err)
			}
		})
	}
}

func TestRunTranscodeRejectsProtocolPaths(t *testing.T) {
	// Validation happens before any process is started, so a nonexistent
	// ffmpeg binary proves no exec occurred.
	job := TranscodeJob{
		FFmpegPath: "/nonexistent/ffmpeg",
		InputFile:  "http://169.254.169.254/latest/meta-data",
		OutputFile: "/tmp/out.mkv",
	}
	if err := RunTranscode(t.Context(), job); err == nil {
		t.Fatalf("expected SSRF input path to be rejected")
	} else if !strings.Contains(err.Error(), "invalid input path") {
		t.Fatalf("unexpected error: %v", err)
	}

	job = TranscodeJob{
		FFmpegPath: "/nonexistent/ffmpeg",
		InputFile:  "/media/movie.mkv",
		OutputFile: "concat:a|b",
	}
	if err := RunTranscode(t.Context(), job); err == nil {
		t.Fatalf("expected protocol output path to be rejected")
	}

	job = TranscodeJob{
		FFmpegPath: "/nonexistent/ffmpeg",
		InputFile:  "-show_frames",
		OutputFile: "/tmp/out.mkv",
	}
	if err := RunTranscode(t.Context(), job); err == nil {
		t.Fatalf("expected flag-like input path to be rejected")
	}
}

func TestRunTranscodeRejectsInvalidCustomFlagsBeforeProcessLaunch(t *testing.T) {
	// Validation happens before any process is started, so a nonexistent
	// ffmpeg binary proves no exec occurred.
	invalidFlags := []string{
		"-preset ultrafast /tmp/extra.mkv",
		"/tmp/extra.mkv -preset ultrafast",
		"-f null /dev/null",
		"-dump_attachment:t:0 /etc/shadow",
		"-bf 4 -",
		"keyint=60 /tmp/evil.mkv",
	}
	for _, flags := range invalidFlags {
		job := TranscodeJob{
			FFmpegPath: "/nonexistent/ffmpeg",
			InputFile:  "/media/movie.mkv",
			OutputFile: "/tmp/out.mkv",
			Spec: jobspec.JobSpec{
				CustomFlags: flags,
			},
		}
		if err := RunTranscode(t.Context(), job); err == nil {
			t.Fatalf("expected custom flags %q to be rejected before process launch", flags)
		} else if !strings.Contains(err.Error(), "invalid custom flags") {
			t.Fatalf("unexpected error for %q: %v", flags, err)
		}
	}
}

func TestRunTranscodeRejectsSymlinkInput(t *testing.T) {
	dir := t.TempDir()
	realFile := filepath.Join(dir, "real.mkv")
	if err := os.WriteFile(realFile, []byte("fake video content"), 0644); err != nil {
		t.Fatalf("failed to write real file: %v", err)
	}
	link := filepath.Join(dir, "link.mkv")
	if err := os.Symlink(realFile, link); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	job := TranscodeJob{
		FFmpegPath: "/nonexistent/ffmpeg",
		InputFile:  link,
		OutputFile: filepath.Join(dir, "out.mkv"),
	}
	if err := RunTranscode(t.Context(), job); err == nil {
		t.Fatalf("expected RunTranscode to reject symlink input file")
	} else if !strings.Contains(err.Error(), "invalid input path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProbeMediaRejectsProtocolPaths(t *testing.T) {
	// ffprobe path is intentionally invalid: rejection must happen before exec.
	badPaths := []string{
		"http://169.254.169.254/x",
		"concat:a|b",
		"-show_frames",
	}
	for _, p := range badPaths {
		if _, err := ProbeMedia("/nonexistent/ffprobe", p); err == nil {
			t.Fatalf("expected path %q to be rejected", p)
		} else if !strings.Contains(err.Error(), "invalid probe path") {
			t.Fatalf("unexpected error for %q: %v", p, err)
		}
	}
}
