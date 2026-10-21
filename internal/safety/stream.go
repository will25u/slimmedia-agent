package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MediaServerPresence records which media servers exist on this machine,
// discovered by ProbeMediaServerPresence at agent startup. The stream-safety
// check only consults servers that are present: a server that is not
// installed or not running cannot be streaming the target file, so checking
// it (and failing the job when it refuses connections) is wrong.
type MediaServerPresence struct {
	Plex     ServerPresence
	Jellyfin ServerPresence
	Emby     ServerPresence
}

// ServerPresence describes one media server.
type ServerPresence struct {
	// Configured is true when the operator explicitly pointed the agent at
	// this server (PLEX_URL / JELLYFIN_URL / EMBY_URL).
	Configured bool `json:"configured"`
	// Reachable is true when the server answered its health endpoint during
	// the startup probe.
	Reachable bool `json:"reachable"`
	// URL is the base URL the agent queries for this server.
	URL string `json:"url,omitempty"`
}

// Available reports whether this server should be consulted for stream
// conflicts: either the operator configured it explicitly or the startup
// probe saw it alive. For available servers, check errors still fail the
// job (fail closed); unavailable servers are skipped entirely.
func (p ServerPresence) Available() bool { return p.Configured || p.Reachable }

// mediaProbeTimeout bounds each server's health probe at startup.
const mediaProbeTimeout = 2 * time.Second

// ProbeMediaServerPresence discovers which media servers are installed and
// running on this machine by querying each product's unauthenticated health
// endpoint. The result should be captured once at agent startup and reused
// for the capabilities report and every job's stream-safety check.
func ProbeMediaServerPresence() MediaServerPresence {
	return probeMediaServerPresence(&http.Client{Timeout: mediaProbeTimeout})
}

func probeMediaServerPresence(client *http.Client) MediaServerPresence {
	plex, jellyfin, emby := resolveURLs()

	// Emby shares Jellyfin's default port; when both resolve to the same
	// URL, the Emby API lives under /emby.
	embyHealthPath := "/System/Info/Public"
	if strings.TrimRight(emby, "/") == strings.TrimRight(jellyfin, "/") {
		embyHealthPath = "/emby/System/Info/Public"
	}

	return MediaServerPresence{
		Plex:     probeServer(client, plex, os.Getenv("PLEX_URL") != "", "/identity"),
		Jellyfin: probeServer(client, jellyfin, os.Getenv("JELLYFIN_URL") != "", "/System/Info/Public"),
		Emby:     probeServer(client, emby, os.Getenv("EMBY_URL") != "", embyHealthPath),
	}
}

func probeServer(client *http.Client, baseURL string, configured bool, healthPath string) ServerPresence {
	p := ServerPresence{Configured: configured, URL: strings.TrimRight(baseURL, "/")}
	resp, err := client.Get(p.URL + healthPath)
	if err != nil {
		return p
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	p.Reachable = resp.StatusCode == http.StatusOK
	return p
}

// resolveURLs returns the Plex, Jellyfin, and Emby base URLs from the
// environment, falling back to each product's default localhost port.
func resolveURLs() (plex, jellyfin, emby string) {
	plex = os.Getenv("PLEX_URL")
	if plex == "" {
		plex = "http://localhost:32400"
	}
	jellyfin = os.Getenv("JELLYFIN_URL")
	if jellyfin == "" {
		jellyfin = "http://localhost:8096"
	}
	emby = os.Getenv("EMBY_URL")
	if emby == "" {
		emby = "http://localhost:8096"
	}
	return plex, jellyfin, emby
}

type MediaServerChecker struct {
	Client        *http.Client
	PlexURL       string
	JellyfinURL   string
	EmbyURL       string
	PlexToken     string
	JellyfinToken string
	EmbyToken     string
	// Presence restricts checks to servers detected at startup (see
	// ProbeMediaServerPresence). Servers that are neither configured nor
	// reachable are skipped. When nil, all servers are checked.
	Presence *MediaServerPresence
	// Logf receives non-fatal skip notices; nil discards them.
	Logf func(format string, args ...any)
}

func NewMediaServerChecker() *MediaServerChecker {
	plex, jellyfin, emby := resolveURLs()

	return &MediaServerChecker{
		Client: &http.Client{
			Timeout: 1 * time.Second,
		},
		PlexURL:       strings.TrimRight(plex, "/"),
		JellyfinURL:   strings.TrimRight(jellyfin, "/"),
		EmbyURL:       strings.TrimRight(emby, "/"),
		PlexToken:     os.Getenv("PLEX_TOKEN"),
		JellyfinToken: os.Getenv("JELLYFIN_TOKEN"),
		EmbyToken:     os.Getenv("EMBY_TOKEN"),
	}
}

// serverAvailable reports whether the named server should be consulted,
// given startup probe results. A nil Presence means no probe information,
// in which case every server is checked.
func (c *MediaServerChecker) serverAvailable(name string) bool {
	if c.Presence == nil {
		return true
	}
	var p ServerPresence
	switch name {
	case "Plex Media Server":
		p = c.Presence.Plex
	case "Jellyfin Server":
		p = c.Presence.Jellyfin
	case "Emby Server":
		p = c.Presence.Emby
	default:
		return true
	}
	if p.Available() {
		return true
	}
	c.logf("stream-safety: skipping %s (not detected on this machine)", name)
	return false
}

func (c *MediaServerChecker) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *MediaServerChecker) CheckConflict(ctx context.Context, targetFilePath string) (bool, string, error) {
	if targetFilePath == "" {
		return false, "", nil
	}

	targetBase := filepath.Base(targetFilePath)
	targetClean := filepath.Clean(targetFilePath)

	// 1. Check Plex
	if c.serverAvailable("Plex Media Server") {
		if inUse, err := c.checkPlex(ctx, targetClean, targetBase); err != nil {
			return false, "Plex Media Server", err
		} else if inUse {
			return true, "Plex Media Server", nil
		}
	}

	// 2. Check Jellyfin
	if c.serverAvailable("Jellyfin Server") {
		token := c.JellyfinToken
		if token == "" {
			token = os.Getenv("JELLYFIN_TOKEN")
		}
		if inUse, err := c.checkJellyfin(ctx, c.JellyfinURL+"/Sessions", token, targetClean, targetBase); err != nil {
			return false, "Jellyfin Server", err
		} else if inUse {
			return true, "Jellyfin Server", nil
		}
	}

	// 3. Check Emby (if distinct or via /emby/Sessions)
	if c.serverAvailable("Emby Server") {
		token := c.EmbyToken
		if token == "" {
			token = os.Getenv("EMBY_TOKEN")
		}
		embyEndpoint := c.EmbyURL + "/Sessions"
		if c.EmbyURL == c.JellyfinURL {
			embyEndpoint = c.EmbyURL + "/emby/Sessions"
		}
		if inUse, err := c.checkJellyfin(ctx, embyEndpoint, token, targetClean, targetBase); err != nil {
			return false, "Emby Server", err
		} else if inUse {
			return true, "Emby Server", nil
		}
	}

	return false, "", nil
}

func (c *MediaServerChecker) checkPlex(ctx context.Context, targetClean, targetBase string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.PlexURL+"/status/sessions", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	token := c.PlexToken
	if token == "" {
		token = os.Getenv("PLEX_TOKEN")
	}
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.logf("stream-safety: Plex returned unauthenticated status %d; skipping conflict check", resp.StatusCode)
		return false, nil
	}

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status code from Plex: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}

	type plexJSON struct {
		MediaContainer struct {
			Metadata []struct {
				Media []struct {
					Part []struct {
						File string `json:"file"`
					} `json:"Part"`
				} `json:"Media"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}

	var pj plexJSON
	if err := json.Unmarshal(bodyBytes, &pj); err == nil && len(pj.MediaContainer.Metadata) > 0 {
		for _, m := range pj.MediaContainer.Metadata {
			for _, med := range m.Media {
				for _, part := range med.Part {
					if isMatch(part.File, targetClean, targetBase) {
						return true, nil
					}
				}
			}
		}
	} else if err != nil {
		return false, fmt.Errorf("failed to parse plex json: %v", err)
	}

	bodyStr := string(bodyBytes)
	if strings.Contains(bodyStr, targetClean) || (targetBase != "" && strings.Contains(bodyStr, targetBase)) {
		return true, nil
	}

	return false, nil
}

func (c *MediaServerChecker) checkJellyfin(ctx context.Context, endpoint, token, targetClean, targetBase string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("X-Emby-Token", token)
		req.Header.Set("X-MediaBrowser-Token", token)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.logf("stream-safety: Jellyfin/Emby returned unauthenticated status %d; skipping conflict check", resp.StatusCode)
		return false, nil
	}

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status code from Jellyfin/Emby: %d", resp.StatusCode)
	}

	var sessions []struct {
		NowPlayingItem struct {
			Path string `json:"Path"`
			Name string `json:"Name"`
		} `json:"NowPlayingItem"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&sessions); err == nil {
		for _, s := range sessions {
			if isMatch(s.NowPlayingItem.Path, targetClean, targetBase) {
				return true, nil
			}
		}
	} else {
		return false, fmt.Errorf("failed to parse session json: %v", err)
	}

	return false, nil
}

func isMatch(sessionFile, targetClean, targetBase string) bool {
	if sessionFile == "" {
		return false
	}
	cleaned := filepath.Clean(sessionFile)
	if cleaned == targetClean {
		return true
	}
	if filepath.Base(cleaned) == targetBase {
		return true
	}
	return false
}
