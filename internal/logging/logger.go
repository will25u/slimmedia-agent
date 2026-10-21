package logging

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxSizeBytes int64         = 100 * 1024 * 1024  // 100 MB
	DefaultMaxAge       time.Duration = 7 * 24 * time.Hour // 7 days
)

// RotatingFile is a thread-safe rotating file writer.
type RotatingFile struct {
	mu           sync.Mutex
	filePath     string
	maxSizeBytes int64
	maxAge       time.Duration
	file         *os.File
	currentSize  int64
}

// NewRotatingFile creates or opens a rotating log file.
func NewRotatingFile(filePath string, maxSizeBytes int64, maxAge time.Duration) (*RotatingFile, error) {
	if maxSizeBytes <= 0 {
		maxSizeBytes = DefaultMaxSizeBytes
	}
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}

	dir := filepath.Dir(filePath)
	// Logs can contain operational metadata (paths, job IDs); the directory
	// and files must not be world-readable.
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	rf := &RotatingFile{
		filePath:     filePath,
		maxSizeBytes: maxSizeBytes,
		maxAge:       maxAge,
	}

	if err := rf.openCurrentFile(); err != nil {
		return nil, err
	}

	return rf, nil
}

func (rf *RotatingFile) openCurrentFile() error {
	info, err := os.Stat(rf.filePath)
	if err == nil {
		rf.currentSize = info.Size()
	} else if os.IsNotExist(err) {
		rf.currentSize = 0
	} else {
		return err
	}

	f, err := os.OpenFile(rf.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return err
	}
	// Tighten files created by older builds (0644) — and rotated archives
	// renamed from them — to group-readable-only.
	_ = os.Chmod(rf.filePath, 0640)
	rf.file = f
	return nil
}

// Write writes data to the rotating log file, rotating if the size limit is exceeded.
func (rf *RotatingFile) Write(p []byte) (n int, err error) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	writeLen := int64(len(p))
	if rf.file == nil || rf.currentSize+writeLen > rf.maxSizeBytes {
		if err := rf.rotate(); err != nil {
			// If rotation fails, attempt to write to existing file anyway
			if rf.file == nil {
				return 0, err
			}
		}
	}

	n, err = rf.file.Write(p)
	rf.currentSize += int64(n)
	return n, err
}

func (rf *RotatingFile) rotate() error {
	if rf.file != nil {
		_ = rf.file.Sync()
		_ = rf.file.Close()
		rf.file = nil
	}

	// Rename current file to timestamped archive.
	// If a rotation has already occurred in the same second, append a sequence counter
	// to avoid overwriting existing archives.
	now := time.Now().UTC()
	timestamp := now.Format("20060102-150405")
	backupPath := fmt.Sprintf("%s.%s", rf.filePath, timestamp)
	if _, err := os.Stat(backupPath); err == nil {
		for seq := 1; ; seq++ {
			candidate := fmt.Sprintf("%s.%s.%d", rf.filePath, timestamp, seq)
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				backupPath = candidate
				break
			}
		}
	}

	if err := os.Rename(rf.filePath, backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to rotate log file: %w", err)
	}
	_ = os.Chmod(backupPath, 0640)

	// Clean up archives older than maxAge
	rf.cleanOldLogs()

	return rf.openCurrentFile()
}

func (rf *RotatingFile) cleanOldLogs() {
	dir := filepath.Dir(rf.filePath)
	base := filepath.Base(rf.filePath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, base+".") {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if now.Sub(info.ModTime()) > rf.maxAge {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
	}
}

// Close flushes and closes the rotating log file.
func (rf *RotatingFile) Close() error {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.file != nil {
		err := rf.file.Close()
		rf.file = nil
		return err
	}
	return nil
}

// SetupLogger initializes structured slog JSON logging to both stdout and a rotating log file.
func SetupLogger(serviceName, logDir string) (*slog.Logger, io.Closer, error) {
	var writers []io.Writer
	writers = append(writers, os.Stdout)

	var closer io.Closer
	if logDir != "" {
		logFile := filepath.Join(logDir, serviceName+".log")
		rf, err := NewRotatingFile(logFile, DefaultMaxSizeBytes, DefaultMaxAge)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to setup rotating file logger: %w", err)
		}
		writers = append(writers, rf)
		closer = rf
	}

	multiWriter := io.MultiWriter(writers...)
	handler := slog.NewJSONHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})

	logger := slog.New(handler).With("service", serviceName)
	slog.SetDefault(logger)

	// Direct standard library log output into slog
	log.SetFlags(0)
	log.SetOutput(&slogWriter{logger: logger})

	return logger, closer, nil
}

type slogWriter struct {
	logger *slog.Logger
}

func (w *slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		w.logger.Info(msg)
	}
	return len(p), nil
}
