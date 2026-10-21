package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func TestCleanLocalPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		unixOnly bool
	}{
		{
			name:     "empty path",
			input:    "",
			expected: "",
		},
		{
			name:     "whitespace only",
			input:    "   \t  ",
			expected: "",
		},
		{
			name:     "simple path with trailing slashes",
			input:    "/mnt/media/movies///",
			expected: "/mnt/media/movies",
		},
		{
			name:     "root path",
			input:    "/",
			expected: "/",
		},
		{
			name:     "shell escaped spaces and ampersand",
			input:    `/mnt/z/Movies\ \&\ TV\ Shows/`,
			expected: "/mnt/z/Movies & TV Shows",
			unixOnly: true,
		},
		{
			name:     "shell escaped parentheses",
			input:    `/mnt/media/Sci-Fi\ \(2024\)/`,
			expected: "/mnt/media/Sci-Fi (2024)",
			unixOnly: true,
		},
		{
			name:     "shell escaped brackets and quotes",
			input:    `/mnt/media/\[Remux\]\ \'Special\'/`,
			expected: "/mnt/media/[Remux] 'Special'",
			unixOnly: true,
		},
		{
			name:     "mixed separators normalized on unix",
			input:    `/mnt/media\movies\action/film.mkv`,
			expected: "/mnt/media/movies/action/film.mkv",
			unixOnly: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unixOnly && runtime.GOOS == "windows" {
				t.Skip("skipping Unix-specific test on Windows")
			}
			got := CleanLocalPath(tc.input)
			if got != tc.expected {
				t.Errorf("CleanLocalPath(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestValidateLocalLibraryDirectory(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("valid directory", func(t *testing.T) {
		valid, reason := ValidateLocalLibraryDirectory(tempDir)
		if !valid || reason != "" {
			t.Errorf("expected valid directory, got valid=%v, reason=%q", valid, reason)
		}
	})

	t.Run("non-existent directory", func(t *testing.T) {
		missingDir := filepath.Join(tempDir, "does_not_exist")
		valid, reason := ValidateLocalLibraryDirectory(missingDir)
		if valid || reason != "Directory does not exist" {
			t.Errorf("expected not valid with 'Directory does not exist', got valid=%v, reason=%q", valid, reason)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		valid, reason := ValidateLocalLibraryDirectory("")
		if valid || reason != "Directory does not exist" {
			t.Errorf("expected not valid with 'Directory does not exist', got valid=%v, reason=%q", valid, reason)
		}
	})

	t.Run("file instead of directory", func(t *testing.T) {
		filePath := filepath.Join(tempDir, "file.txt")
		if err := os.WriteFile(filePath, []byte("hello"), 0644); err != nil {
			t.Fatalf("failed to create file: %v", err)
		}
		valid, reason := ValidateLocalLibraryDirectory(filePath)
		if valid || reason != "Path is not a directory" {
			t.Errorf("expected not valid with 'Path is not a directory', got valid=%v, reason=%q", valid, reason)
		}
	})
}

func TestValidateJobFile_ShellEscapedLocalPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping Unix shell escape test on Windows")
	}

	tempDir := t.TempDir()
	actualDir := filepath.Join(tempDir, "Movies & TV Shows")
	if err := os.MkdirAll(actualDir, 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	testFile := filepath.Join(actualDir, "movie.mkv")
	if err := os.WriteFile(testFile, []byte("dummy video"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Shell-escaped path with backslashes
	escapedLocalPath := filepath.Join(tempDir, `Movies\ \&\ TV\ Shows`) + "/"

	canonical, err := ValidateJobFile(escapedLocalPath, "movie.mkv", "")
	if err != nil {
		t.Fatalf("ValidateJobFile failed with shell escaped localPath: %v", err)
	}
	if canonical != testFile {
		t.Errorf("ValidateJobFile = %q, want %q", canonical, testFile)
	}
}

func TestDaemon_SetLibraryPath_ValidationAndReporting(t *testing.T) {
	srv := newFakeServer(t)
	cfg := testResolvedConfig(t, srv)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg.ConfigPath, []byte(`{"api_key":"test-key","server":"`+srv.ts.URL+`"}`), 0600); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	waitForEvent(t, d.Events(), func(ev Event) bool {
		return ev.Type == EventConnection && ev.Connected
	})

	t.Run("invalid path triggers LIBRARY_PATH_ERROR and marks unmounted", func(t *testing.T) {
		invalidPath := filepath.Join(t.TempDir(), "missing_dir")
		err := d.SetLibraryPath("lib-bad", invalidPath)
		if err == nil {
			t.Fatalf("expected error from invalid SetLibraryPath, got nil")
		}

		// Verify supervisor and worker media dirs are unmounted
		if d.worker.GetMediaDir() != "" {
			t.Errorf("worker.GetMediaDir() = %q, want empty", d.worker.GetMediaDir())
		}
		if d.jobs.GetMediaDir() != "" {
			t.Errorf("jobs.GetMediaDir() = %q, want empty", d.jobs.GetMediaDir())
		}
		if d.jobs.GetMountError() != "Directory does not exist" {
			t.Errorf("jobs.GetMountError() = %q, want 'Directory does not exist'", d.jobs.GetMountError())
		}

		// Verify WebSocket error event was sent
		var foundErrEnv bool
		var errPayload protocol.LibraryPathStatusPayload
		for {
			select {
			case env := <-srv.incoming:
				if env.Type == protocol.TypeLibraryPathError {
					foundErrEnv = true
					_ = json.Unmarshal(env.Payload, &errPayload)
					break
				}
			case <-time.After(500 * time.Millisecond):
				break
			}
			if foundErrEnv {
				break
			}
		}
		if !foundErrEnv {
			t.Errorf("expected LIBRARY_PATH_ERROR envelope on server incoming channel")
		} else {
			if errPayload.LibraryID != "lib-bad" || errPayload.Valid || errPayload.Reason != "Directory does not exist" {
				t.Errorf("unexpected errPayload: %+v", errPayload)
			}
		}

		// Verify future job fails fast with descriptive error
		err = d.jobs.StartJob(context.Background(), protocol.StartJobPayload{
			JobID:        "job-fail-fast",
			InputPath:    "/any/path.mkv",
			RelativePath: "movie.mkv",
		}, nil)
		if err == nil {
			t.Fatalf("expected StartJob to fail fast, got nil")
		}
		if !strings.Contains(err.Error(), "ErrLibraryNotMounted: Directory does not exist") {
			t.Errorf("expected descriptive error containing 'ErrLibraryNotMounted: Directory does not exist', got %v", err)
		}
	})

	t.Run("valid path triggers LIBRARY_PATH_VALID and mounts successfully", func(t *testing.T) {
		validDir := t.TempDir()
		err := d.SetLibraryPath("lib-good", validDir)
		if err != nil {
			t.Fatalf("unexpected error from valid SetLibraryPath: %v", err)
		}

		if d.worker.GetMediaDir() != validDir {
			t.Errorf("worker.GetMediaDir() = %q, want %q", d.worker.GetMediaDir(), validDir)
		}
		if d.jobs.GetMediaDir() != validDir {
			t.Errorf("jobs.GetMediaDir() = %q, want %q", d.jobs.GetMediaDir(), validDir)
		}
		if d.jobs.GetMountError() != "" {
			t.Errorf("jobs.GetMountError() = %q, want empty", d.jobs.GetMountError())
		}

		// Verify WebSocket success event was sent
		var foundValidEnv bool
		var validPayload protocol.LibraryPathStatusPayload
		for {
			select {
			case env := <-srv.incoming:
				if env.Type == protocol.TypeLibraryPathValid {
					foundValidEnv = true
					_ = json.Unmarshal(env.Payload, &validPayload)
					break
				}
			case <-time.After(500 * time.Millisecond):
				break
			}
			if foundValidEnv {
				break
			}
		}
		if !foundValidEnv {
			t.Errorf("expected LIBRARY_PATH_VALID envelope on server incoming channel")
		} else {
			if validPayload.LibraryID != "lib-good" || !validPayload.Valid || validPayload.Status != "mounted" {
				t.Errorf("unexpected validPayload: %+v", validPayload)
			}
		}
	})
}
