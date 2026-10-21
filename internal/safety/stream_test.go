package safety_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/safety"
)

// closedURL returns a URL that is guaranteed to refuse connections.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

func TestProbeMediaServerPresenceNoneReachable(t *testing.T) {
	dead := closedURL(t)
	t.Setenv("PLEX_URL", dead)
	t.Setenv("JELLYFIN_URL", dead)
	t.Setenv("EMBY_URL", dead)

	p := safety.ProbeMediaServerPresence()

	for name, s := range map[string]safety.ServerPresence{
		"Plex":     p.Plex,
		"Jellyfin": p.Jellyfin,
		"Emby":     p.Emby,
	} {
		if s.Reachable {
			t.Errorf("%s: expected not reachable, got %+v", name, s)
		}
		if !s.Configured {
			t.Errorf("%s: expected configured (env URL set), got %+v", name, s)
		}
		// Configured counts as available: operators who set an env URL are
		// consulted even when the server is down (fail closed at job time).
		if !s.Available() {
			t.Errorf("%s: expected configured server to be available, got %+v", name, s)
		}
	}
}

func TestProbeMediaServerPresenceDetectsServers(t *testing.T) {
	var plexHits, jellyfinHits, embyHits atomic.Int32

	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/identity" {
			http.NotFound(w, r)
			return
		}
		plexHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer plex.Close()

	jellyfin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info/Public" {
			http.NotFound(w, r)
			return
		}
		jellyfinHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer jellyfin.Close()

	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info/Public" {
			http.NotFound(w, r)
			return
		}
		embyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer emby.Close()

	t.Setenv("PLEX_URL", plex.URL)
	t.Setenv("JELLYFIN_URL", jellyfin.URL)
	t.Setenv("EMBY_URL", emby.URL)

	p := safety.ProbeMediaServerPresence()

	if !p.Plex.Reachable || p.Plex.URL != plex.URL || !p.Plex.Configured {
		t.Errorf("unexpected Plex presence: %+v", p.Plex)
	}
	if !p.Jellyfin.Reachable || p.Jellyfin.URL != jellyfin.URL {
		t.Errorf("unexpected Jellyfin presence: %+v", p.Jellyfin)
	}
	if !p.Emby.Reachable || p.Emby.URL != emby.URL {
		t.Errorf("unexpected Emby presence: %+v", p.Emby)
	}
	if plexHits.Load() != 1 || jellyfinHits.Load() != 1 || embyHits.Load() != 1 {
		t.Errorf("expected one health hit per server, got plex=%d jellyfin=%d emby=%d",
			plexHits.Load(), jellyfinHits.Load(), embyHits.Load())
	}
}

// Regression test for jobs failing with "stream-safety check failed on Plex
// Media Server: ... connection refused" on machines with no media server
// running: undetected servers must be skipped, not treated as errors.
func TestCheckConflictSkipsUndetectedServers(t *testing.T) {
	target := "/media/movies/Top Gun (1986).mkv"
	var plexHits atomic.Int32

	// Plex mock that WOULD report a streaming conflict if it were consulted.
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plexHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"file":%q}]}]}]}}`, target)
	}))
	defer plex.Close()

	t.Setenv("PLEX_URL", plex.URL)
	// Jellyfin/Emby left at defaults; the presence below marks them absent.

	presence := safety.MediaServerPresence{
		Plex:     safety.ServerPresence{URL: plex.URL},
		Jellyfin: safety.ServerPresence{URL: "http://localhost:8096"},
		Emby:     safety.ServerPresence{URL: "http://localhost:8096"},
	}

	checker := safety.NewMediaServerChecker()
	checker.Presence = &presence

	conflict, serverName, err := checker.CheckConflict(context.Background(), target)
	if err != nil {
		t.Fatalf("expected no error for undetected servers, got: %v", err)
	}
	if conflict {
		t.Fatalf("expected no conflict from undetected servers (server=%s)", serverName)
	}
	if plexHits.Load() != 0 {
		t.Fatalf("undetected Plex server was still queried %d times", plexHits.Load())
	}
}

// A server the operator explicitly configured must still be consulted even if
// the health probe failed, and a failed check on it fails the job (fail
// closed, per issue #49).
func TestCheckConflictFailsClosedForConfiguredServerDown(t *testing.T) {
	dead := closedURL(t)
	t.Setenv("PLEX_URL", dead)
	t.Setenv("JELLYFIN_URL", dead)
	t.Setenv("EMBY_URL", dead)

	presence := safety.ProbeMediaServerPresence()
	if !presence.Plex.Available() {
		t.Fatalf("expected configured-but-down Plex to be available, got %+v", presence.Plex)
	}

	checker := safety.NewMediaServerChecker()
	checker.Presence = &presence

	_, serverName, err := checker.CheckConflict(context.Background(), "/media/movie.mkv")
	if err == nil {
		t.Fatalf("expected fail-closed error for configured-but-down server")
	}
	if serverName != "Plex Media Server" {
		t.Fatalf("expected error attributed to Plex, got %q", serverName)
	}
}

func TestCheckConflictDetectsConflictOnDetectedServer(t *testing.T) {
	target := "/media/movies/Alien (1979).mkv"

	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"file":%q}]}]}]}}`, target)
	}))
	defer plex.Close()

	t.Setenv("PLEX_URL", plex.URL)

	presence := safety.MediaServerPresence{
		Plex:     safety.ServerPresence{URL: plex.URL, Reachable: true},
		Jellyfin: safety.ServerPresence{URL: "http://localhost:8096"},
		Emby:     safety.ServerPresence{URL: "http://localhost:8096"},
	}

	checker := safety.NewMediaServerChecker()
	checker.Presence = &presence

	conflict, serverName, err := checker.CheckConflict(context.Background(), target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !conflict || serverName != "Plex Media Server" {
		t.Fatalf("expected conflict on Plex, got conflict=%v server=%s", conflict, serverName)
	}
}

func TestCheckConflictJellyfinEmbyTokenAuth(t *testing.T) {
	target := "/media/movies/The Matrix (1999).mkv"
	var receivedJFToken, receivedEmbyToken string

	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJFToken = r.Header.Get("X-Emby-Token")
		if receivedJFToken != "jf-secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"NowPlayingItem":{"Path":%q,"Name":"The Matrix"}}]`, target)
	}))
	defer jf.Close()

	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedEmbyToken = r.Header.Get("X-Emby-Token")
		if receivedEmbyToken != "emby-secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"NowPlayingItem":{"Path":%q,"Name":"The Matrix"}}]`, target)
	}))
	defer emby.Close()

	t.Setenv("JELLYFIN_URL", jf.URL)
	t.Setenv("JELLYFIN_TOKEN", "jf-secret-token")
	t.Setenv("EMBY_URL", emby.URL)
	t.Setenv("EMBY_TOKEN", "emby-secret-token")

	presence := safety.MediaServerPresence{
		Plex:     safety.ServerPresence{URL: "http://localhost:32400"},
		Jellyfin: safety.ServerPresence{URL: jf.URL, Reachable: true},
		Emby:     safety.ServerPresence{URL: emby.URL, Reachable: true},
	}

	checker := safety.NewMediaServerChecker()
	checker.Presence = &presence

	conflict, serverName, err := checker.CheckConflict(context.Background(), target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !conflict || serverName != "Jellyfin Server" {
		t.Fatalf("expected conflict on Jellyfin, got conflict=%v server=%s", conflict, serverName)
	}
	if receivedJFToken != "jf-secret-token" {
		t.Errorf("expected X-Emby-Token %q on Jellyfin, got %q", "jf-secret-token", receivedJFToken)
	}
}

func TestCheckConflictGracefulOnUnauthorized(t *testing.T) {
	target := "/media/movies/The Matrix (1999).mkv"

	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer jf.Close()

	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer emby.Close()

	t.Setenv("JELLYFIN_URL", jf.URL)
	t.Setenv("EMBY_URL", emby.URL)

	presence := safety.MediaServerPresence{
		Plex:     safety.ServerPresence{URL: "http://localhost:32400"},
		Jellyfin: safety.ServerPresence{URL: jf.URL, Reachable: true},
		Emby:     safety.ServerPresence{URL: emby.URL, Reachable: true},
	}

	checker := safety.NewMediaServerChecker()
	checker.Presence = &presence

	conflict, _, err := checker.CheckConflict(context.Background(), target)
	if err != nil {
		t.Fatalf("expected graceful handling of 401/403 without error, got: %v", err)
	}
	if conflict {
		t.Fatalf("expected no conflict on unauthenticated responses, got true")
	}
}

