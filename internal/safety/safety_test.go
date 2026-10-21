package safety

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func TestIsQuietHoursActive(t *testing.T) {
	// 1. When disabled
	schedDisabled := &protocol.ScheduleConfig{
		QuietHoursEnabled: false,
		ScheduleGrid:      protocol.WeeklySchedule(makeTestGrid(2, 8)),
		Timezone:          "UTC",
	}
	utcTime := time.Date(2026, 9, 11, 4, 30, 0, 0, time.UTC)
	if IsQuietHoursActive(utcTime, schedDisabled) {
		t.Errorf("expected false when quiet hours disabled, got true")
	}

	// 2. Daytime window (02:00 to 08:00 UTC)
	schedEnabled := &protocol.ScheduleConfig{
		QuietHoursEnabled: true,
		ScheduleGrid:      protocol.WeeklySchedule(makeTestGrid(22, 6)),
		Timezone:          "UTC",
	}
	// 04:30 UTC -> inside
	if !IsQuietHoursActive(time.Date(2026, 9, 11, 4, 30, 0, 0, time.UTC), schedEnabled) {
		t.Errorf("expected true for 04:30 in 02:00-08:00 window, got false")
	}
	// 09:15 UTC -> outside
	if IsQuietHoursActive(time.Date(2026, 9, 11, 9, 15, 0, 0, time.UTC), schedEnabled) {
		t.Errorf("expected false for 09:15 in 02:00-08:00 window, got true")
	}

	// 3. Overnight window (22:00 to 06:00 UTC)
	schedOvernight := &protocol.ScheduleConfig{
		QuietHoursEnabled: true,
		ScheduleGrid:      protocol.WeeklySchedule(makeTestGrid(0, 0)),
		Timezone:          "UTC",
	}
	// 23:30 UTC -> inside
	if !IsQuietHoursActive(time.Date(2026, 9, 11, 23, 30, 0, 0, time.UTC), schedOvernight) {
		t.Errorf("expected true for 23:30 in 22:00-06:00 window, got false")
	}
	// 03:00 UTC -> inside
	if !IsQuietHoursActive(time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC), schedOvernight) {
		t.Errorf("expected true for 03:00 in 22:00-06:00 window, got false")
	}
}

func TestMediaServerStreamingConflict(t *testing.T) {
	// Mock Jellyfin server
	activeMedia := "/media/movies/Avatar (2009).mkv"
	jfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Sessions" {
			sessions := []map[string]interface{}{
				{
					"NowPlayingItem": map[string]interface{}{
						"Path": activeMedia,
						"Name": "Avatar",
					},
				},
			}
			json.NewEncoder(w).Encode(sessions)
			return
		}
		http.NotFound(w, r)
	}))
	defer jfServer.Close()

	plexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"MediaContainer": {"Metadata": []}}`))
	}))
	defer plexServer.Close()

	embyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer embyServer.Close()

	checker := &MediaServerChecker{
		Client:      jfServer.Client(), // httptest clients can talk to any httptest.Server
		PlexURL:     plexServer.URL,
		JellyfinURL: jfServer.URL,
		EmbyURL:     embyServer.URL,
	}

	// 1. Conflict on matching file path
	conflict, server, err := checker.CheckConflict(context.Background(), "/media/movies/Avatar (2009).mkv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !conflict || server != "Jellyfin Server" {
		t.Errorf("expected conflict on Jellyfin Server, got conflict=%v, server=%s", conflict, server)
	}

	// 2. Conflict on matching basename
	conflict, server, err = checker.CheckConflict(context.Background(), "/mnt/storage/Avatar (2009).mkv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !conflict {
		t.Errorf("expected conflict on matching basename, got false")
	}

	// 3. No conflict on different file
	conflict, server, err = checker.CheckConflict(context.Background(), "/media/movies/Interstellar (2014).mkv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conflict {
		t.Errorf("expected no conflict on different file, got conflict=%v on %s", conflict, server)
	}
}

func makeTestGrid(startHour, endHour int) string {
	b := []byte(strings.Repeat("0", 168))
	for day := 0; day < 7; day++ {
		for h := 0; h < 24; h++ {
			if startHour < endHour {
				if h >= startHour && h < endHour {
					b[day*24+h] = '1'
				}
			} else {
				if h >= startHour || h < endHour {
					b[day*24+h] = '1'
				}
			}
		}
	}
	return string(b)
}
