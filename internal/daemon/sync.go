package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/scanner"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// syncMediaLibrary scans the media directory, records the discovered files
// in the local confinement registry, and uploads the catalog to the server.
// Scan progress is reported both as agent status and scan.progress envelopes.
func (d *Daemon) syncMediaLibrary(mediaDir string, targetLibraryID ...string) {
	d.scanMu.Lock()
	defer d.scanMu.Unlock()

	d.mu.Lock()
	libraryID := ""
	dataDir := ""
	apiKey := ""
	if d.cfg != nil {
		libraryID = d.cfg.LibraryID
		dataDir = d.cfg.DataDir
		apiKey = d.cfg.APIKey
	}
	d.mu.Unlock()

	if len(targetLibraryID) > 0 && targetLibraryID[0] != "" {
		libraryID = targetLibraryID[0]
	}

	isTranscodingActive := func() bool {
		jobs := d.getJobs()
		return jobs != nil && jobs.ActiveCount() > 0
	}

	if !isTranscodingActive() {
		d.setStatus("SCANNING", "Initializing media scan...")
	}
	var scanCompleted bool
	defer func() {
		if isTranscodingActive() {
			d.setStatus("WORKING", "")
		} else {
			d.setStatus("IDLE", "")
		}
		if !scanCompleted && d.wsClient != nil {
			currentLibID := libraryID
			if currentLibID == "" {
				d.mu.Lock()
				if d.cfg != nil {
					currentLibID = d.cfg.LibraryID
				}
				d.mu.Unlock()
			}

			payload, _ := json.Marshal(map[string]string{
				"status":     "complete",
				"library_id": currentLibID,
			})
			d.wsClient.Send(protocol.Envelope{
				Type:    protocol.TypeScanComplete,
				ID:      fmt.Sprintf("scan-complete-%d", time.Now().UnixNano()),
				TS:      time.Now(),
				Payload: payload,
			})
		}
	}()

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Printf("Cannot create agent data directory %s: %v (ffprobe/ffmpeg resolution and probe caching will fail)", MaskUserPath(dataDir), err)
	}
	_ = os.Chmod(dataDir, 0700)

	ffprobePath := ffmpeg.ResolveBinaryPath(dataDir, "ffprobe")

	var lastReportMu sync.Mutex
	var lastReportTime time.Time

	items, err := scanner.ScanMediaWithProgress(mediaDir, ffprobePath, func(file string, info ffmpeg.MediaInfo, current int, max int, completed int, total int) {
		isBatch := (total > 0 && (total <= 10 || completed == total)) || completed == 1 || completed%25 == 0
		if isBatch {
			if total > 0 {
				log.Printf("Scanning media [%d/%d] [Concurrency: %d/%d]...", completed, total, current, max)
			} else {
				log.Printf("Scanning media [%d] [Concurrency: %d/%d]...", completed, current, max)
			}
		}

		lastReportMu.Lock()
		now := time.Now()
		shouldSend := isBatch || now.Sub(lastReportTime) >= 500*time.Millisecond || completed == 1 || (total > 0 && completed == total)
		if shouldSend {
			lastReportTime = now
		}
		lastReportMu.Unlock()

		if shouldSend {
			pct := 0.0
			if total > 0 {
				if completed > total {
					total = completed
				}
				pct = (float64(completed) / float64(total)) * 100.0
			}
			fileName := filepath.Base(file)
			var statusMsg string
			if total > 0 {
				statusMsg = fmt.Sprintf("Scanning media [%d/%d]", completed, total)
			} else {
				statusMsg = fmt.Sprintf("Scanning media [%d]", completed)
			}
			if !isTranscodingActive() {
				d.setStatus("SCANNING", fmt.Sprintf("%s [Concurrency: %d/%d]", statusMsg, current, max))
			}

			scanPayload := protocol.ScanProgressPayload{
				Status:         statusMsg,
				Completed:      completed,
				Total:          total,
				Percent:        pct,
				ProgressPct:    pct,
				CurrentWorkers: current,
				MaxWorkers:     max,
				CurrentFile:    fileName,
			}
			b, _ := json.Marshal(scanPayload)
			d.wsClient.Send(protocol.Envelope{
				Type:    protocol.TypeScanProgress,
				ID:      fmt.Sprintf("scan-%d", time.Now().UnixNano()),
				TS:      time.Now(),
				Payload: b,
			})
		}
	})
	if err != nil {
		log.Printf("Scanner failed for %s: %v", mediaDir, err)
		d.emit(Event{Type: EventError, Message: fmt.Sprintf("scanner failed for %s: %v", mediaDir, err)})
		return
	}

	if len(items) == 0 {
		log.Printf("No media files found in %s", mediaDir)
	}

	statusMsg := "Finalizing local registry..."
	if !isTranscodingActive() {
		d.setStatus("SCANNING", statusMsg)
	}
	if d.wsClient != nil {
		scanPayload := protocol.ScanProgressPayload{
			Status:         statusMsg,
			Completed:      len(items),
			Total:          len(items),
			Percent:        100.0,
			ProgressPct:    100.0,
			CurrentWorkers: 0,
		}
		b, _ := json.Marshal(scanPayload)
		d.wsClient.Send(protocol.Envelope{
			Type:    protocol.TypeScanProgress,
			ID:      fmt.Sprintf("scan-%d", time.Now().UnixNano()),
			TS:      time.Now(),
			Payload: b,
		})
	}

	// The scan just re-walked the library, so file-existence information is
	// fresh: drop allow-list rows for deleted/renamed files and cap the
	// completed-transcode history (issue #231).
	pruneLocalRegistries(d.db)

	if len(items) == 0 {
		return
	}

	// Record canonical paths locally so server-pushed jobs can be confined
	// to files the scanner actually discovered.
	recordScannedFiles(d.db, items, mediaDir)

	log.Printf("Found %d media file(s) in %s. Uploading to library sync...", len(items), mediaDir)

	syncSessionID := uuid.New().String()
	canonicalMediaDir, err := filepath.Abs(mediaDir)
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(canonicalMediaDir); err == nil {
			canonicalMediaDir = resolved
		}
	}
	canonicalMediaDir = filepath.Clean(canonicalMediaDir)

	batchSize := 500
	totalItems := len(items)
	totalChunks := (totalItems + batchSize - 1) / batchSize
	if totalChunks == 0 {
		totalChunks = 1
	}

	itemsSynced := 0

	for i := 0; i < totalChunks; i++ {
		start := i * batchSize
		end := start + batchSize
		if end > totalItems {
			end = totalItems
		}
		chunkItems := items[start:end]

		pct := 0.0
		if totalItems > 0 {
			pct = (float64(itemsSynced) / float64(totalItems)) * 100.0
		}

		syncMsg := fmt.Sprintf("Syncing chunk %d of %d...", i+1, totalChunks)
		if !isTranscodingActive() {
			d.setStatus("SCANNING", syncMsg)
		}

		syncPayload := protocol.SyncProgressPayload{
			Chunk:       i + 1,
			TotalChunks: totalChunks,
			ItemsSynced: itemsSynced,
			TotalItems:  totalItems,
			Percent:     pct,
			Stage:       "syncing",
			Message:     syncMsg,
		}
		b, _ := json.Marshal(syncPayload)
		d.wsClient.Send(protocol.Envelope{
			Type:    protocol.TypeSyncProgress,
			ID:      fmt.Sprintf("sync-%d-%d", time.Now().UnixNano(), i),
			TS:      time.Now(),
			Payload: b,
		})

		type syncItem struct {
			FilePath     string  `json:"file_path"`
			RelativePath string  `json:"relative_path,omitempty"`
			Filename     string  `json:"filename"`
			Codec        string  `json:"codec"`
			Duration     float64 `json:"duration"`
			SizeBytes    int64   `json:"size_bytes"`
			Hash         string  `json:"hash"`
			Error        string  `json:"error,omitempty"`
		}

		syncItems := make([]syncItem, len(chunkItems))
		for idx, item := range chunkItems {
			rel := item.RelativePath
			if rel == "" {
				if r, err := scanner.NormalizeRelativePath(mediaDir, item.FilePath); err == nil {
					rel = r
				} else if r, err := filepath.Rel(canonicalMediaDir, item.FilePath); err == nil && filepath.IsLocal(r) {
					rel = filepath.ToSlash(r)
				}
			}
			rel = strings.ReplaceAll(rel, "\\", "/")

			syncItems[idx] = syncItem{
				FilePath:     item.FilePath,
				RelativePath: rel,
				Filename:     item.Filename,
				Codec:        item.Codec,
				Duration:     item.Duration,
				SizeBytes:    item.SizeBytes,
				Hash:         item.Hash,
				Error:        item.Error,
			}
		}

		isFinalBatch := (i == totalChunks-1)
		type chunkedSyncPayload struct {
			LibraryID     string     `json:"library_id"`
			SyncSessionID string     `json:"sync_session_id"`
			IsFinalBatch  bool       `json:"is_final_batch"`
			LocalPath     string     `json:"local_path,omitempty"`
			Items         []syncItem `json:"items"`
		}

		d.mu.Lock()
		serverURL := ""
		if d.cfg != nil {
			serverURL = d.cfg.Server
		}
		d.mu.Unlock()

		payload, err := json.Marshal(chunkedSyncPayload{
			LibraryID:     libraryID,
			SyncSessionID: syncSessionID,
			IsFinalBatch:  isFinalBatch,
			LocalPath:     mediaDir,
			Items:         syncItems,
		})
		if err != nil {
			log.Printf("Failed to encode media items chunk %d: %v", i+1, err)
			return
		}

		req, err := http.NewRequest("POST", strings.TrimRight(serverURL, "/")+"/api/v1/library/sync", bytes.NewBuffer(payload))
		if err != nil {
			log.Printf("Failed to create library sync request chunk %d: %v", i+1, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if d.wsClient != nil {
			if token := d.wsClient.Token(); token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		}
		if req.Header.Get("Authorization") == "" && apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		if apiKey != "" {
			req.Header.Set("X-API-Key", apiKey)
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Library sync request chunk %d failed: %v", i+1, err)
			return
		}
		
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
			resp.Body.Close()
			log.Printf("Library sync error chunk %d (%d): %s", i+1, resp.StatusCode, strings.TrimSpace(string(body)))
			return
		}
		var syncResp struct {
			LibraryID  string `json:"library_id"`
			Reconciled bool   `json:"reconciled"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodyBytes)).Decode(&syncResp); err == nil {
			if syncResp.LibraryID != "" {
				d.mu.Lock()
				if d.cfg != nil && d.cfg.LibraryID != syncResp.LibraryID {
					log.Printf("Reconciled library_id updated from %q to %q", d.cfg.LibraryID, syncResp.LibraryID)
					d.cfg.LibraryID = syncResp.LibraryID
				}
				d.mu.Unlock()
			}
		}
		resp.Body.Close()

		itemsSynced += len(chunkItems)
	}

	log.Printf("Successfully synced %d media item(s) to library", itemsSynced)

	finalMsg := "Sync complete"
	if !isTranscodingActive() {
		d.setStatus("SCANNING", finalMsg)
	}
	finalPayload := protocol.SyncProgressPayload{
		Chunk:       totalChunks,
		TotalChunks: totalChunks,
		ItemsSynced: itemsSynced,
		TotalItems:  totalItems,
		Percent:     100.0,
		Stage:       "complete",
		Message:     finalMsg,
	}
	bFinal, _ := json.Marshal(finalPayload)
	d.wsClient.Send(protocol.Envelope{
		Type:    protocol.TypeSyncProgress,
		ID:      fmt.Sprintf("sync-complete-%d", time.Now().UnixNano()),
		TS:      time.Now(),
		Payload: bFinal,
	})

	scanCompleted = true
	finalLibID := libraryID
	if finalLibID == "" {
		d.mu.Lock()
		if d.cfg != nil {
			finalLibID = d.cfg.LibraryID
		}
		d.mu.Unlock()
	}

	scPayload, _ := json.Marshal(map[string]string{
		"status":     "complete",
		"library_id": finalLibID,
	})
	d.wsClient.Send(protocol.Envelope{
		Type:    protocol.TypeScanComplete,
		ID:      fmt.Sprintf("scan-complete-%d", time.Now().UnixNano()),
		TS:      time.Now(),
		Payload: scPayload,
	})
}
