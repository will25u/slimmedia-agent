package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// postAuditReport uploads the daemon's capabilities report (hardware,
// FFmpeg, and media-server sections) to the dashboard so the server can
// derive per-agent hardware flags (nvenc_hardware, qsv_hardware,
// av1_hardware) without waiting for a manual `audit` run (issue #196).
// Best-effort: failures are logged and never block startup.
func (d *Daemon) postAuditReport(report AuditReport) {
	body, err := json.Marshal(report)
	if err != nil {
		log.Printf("Could not encode audit report: %v", err)
		return
	}

	endpoint := strings.TrimRight(d.cfg.Server, "/") + "/api/v1/agents/audit-report"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		log.Printf("Could not create audit report request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.cfg.APIKey)
	req.Header.Set("X-API-Key", d.cfg.APIKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Audit report upload failed: %v", err)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		log.Printf("Audit report sent to %s", d.cfg.Server)
	default:
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
		log.Printf("Audit report upload rejected (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
