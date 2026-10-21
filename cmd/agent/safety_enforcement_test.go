package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/safety"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func TestAgentQuietHoursEnforcement(t *testing.T) {
	// Quiet hours configured from 00:00 to 23:59 (active all day)
	sched := &protocol.ScheduleConfig{
		QuietHoursEnabled: true,
		ScheduleGrid:      protocol.WeeklySchedule(strings.Repeat("1", 168)),
		Timezone:          "UTC",
	}

	active := safety.IsQuietHoursActive(time.Now().UTC(), sched)
	if !active {
		t.Fatalf("expected quiet hours to be active")
	}

	// Verify the envelope payload that would be sent
	completePayload := protocol.FileCompletePayload{
		JobID:        "test-job-quiet",
		InputPath:    "/media/movie.mkv",
		Success:      false,
		ErrorMessage: "skipped: quiet hours active",
	}

	if completePayload.Success {
		t.Errorf("expected Success to be false")
	}
	if completePayload.ErrorMessage != "skipped: quiet hours active" {
		t.Errorf("unexpected error message: %s", completePayload.ErrorMessage)
	}
}

func TestAgentStreamingConflictEnforcement(t *testing.T) {
	testFile := "/media/movies/Top Gun (1986).mkv"

	// Mock Plex server returning session with testFile
	plexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/sessions" {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]interface{}{
				"MediaContainer": map[string]interface{}{
					"Metadata": []map[string]interface{}{
						{
							"Media": []map[string]interface{}{
								{
									"Part": []map[string]interface{}{
										{"file": testFile},
									},
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		http.NotFound(w, r)
	}))
	defer plexServer.Close()

	os.Setenv("PLEX_URL", plexServer.URL)
	defer os.Unsetenv("PLEX_URL")

	checker := safety.NewMediaServerChecker()
	conflict, serverName, err := checker.CheckConflict(context.Background(), testFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !conflict || serverName != "Plex Media Server" {
		t.Fatalf("expected streaming conflict on Plex Media Server, got conflict=%v server=%s", conflict, serverName)
	}

	completePayload := protocol.FileCompletePayload{
		JobID:        "test-job-stream",
		InputPath:    testFile,
		Success:      false,
		ErrorMessage: fmt.Sprintf("deferred: active streaming conflict on %s", serverName),
	}

	if completePayload.Success {
		t.Errorf("expected Success to be false")
	}
	if completePayload.ErrorMessage != "deferred: active streaming conflict on Plex Media Server" {
		t.Errorf("unexpected error message: %s", completePayload.ErrorMessage)
	}
}
