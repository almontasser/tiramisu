package mediaserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"tiramisu/internal/catalog"
)

// Client refreshes a media server library section. Each implementation decides what it
// needs: Plex addresses one section and does nothing without an ID, Jellyfin refreshes
// everything and ignores the argument, so callers pass whatever they have and let the
// client skip the call when it cannot make it.
type Client interface {
	RefreshLibrary(ctx context.Context, sectionID int) error
}

// KindRefresher is a client that can refresh the one library holding a kind of
// content: "movies", "tv" or "anime".
type KindRefresher interface {
	RefreshKind(ctx context.Context, kind string) error
}

// Refresh refreshes the library holding kind when the client can address it,
// and the section otherwise.
func Refresh(ctx context.Context, c Client, kind string, sectionID int) error {
	if kr, ok := c.(KindRefresher); ok && kind != "" {
		return kr.RefreshKind(ctx, kind)
	}
	return c.RefreshLibrary(ctx, sectionID)
}

// New creates the appropriate client based on server type.
func New(serverType, url, token string) Client {
	switch serverType {
	case "jellyfin":
		return &JellyfinClient{
			http:  catalog.NewClient(15 * time.Second),
			URL:   url,
			Token: token,
		}
	default:
		return &PlexClient{
			http:  catalog.NewClient(15 * time.Second),
			URL:   url,
			Token: token,
		}
	}
}

// PlexClient implements Client for Plex Media Server.
type PlexClient struct {
	http  *http.Client
	URL   string
	Token string
}

// RefreshLibrary triggers a Plex library scan. A section ID of 0 means "not configured",
// the documented way to leave the refresh off.
func (c *PlexClient) RefreshLibrary(ctx context.Context, sectionID int) error {
	if c.URL == "" || c.Token == "" || sectionID <= 0 {
		return nil
	}

	urlStr := fmt.Sprintf("%s/library/sections/%d/refresh?X-Plex-Token=%s", c.URL, sectionID, c.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("plex refresh: status %d", resp.StatusCode)
	}

	return nil
}

// JellyfinClient implements Client for Jellyfin Media Server.
type JellyfinClient struct {
	http  *http.Client
	URL   string
	Token string
}

// RefreshLibrary triggers a Jellyfin scan of every library.
func (c *JellyfinClient) RefreshLibrary(ctx context.Context, _ int) error {
	if c.URL == "" || c.Token == "" {
		return nil
	}
	return c.post(ctx, "/Library/Refresh")
}

// RefreshKind scans only the Jellyfin library with a folder named for kind,
// such as ".../library/tv". RefreshLibrary's POST /Library/Refresh restarts
// Jellyfin's Scan Media Library task over every library, and each burst of adds
// cancelled the scan that was running: a 38-minute scan was cancelled before it
// could finish. A library refresh runs beside that task and probes a dozen files
// at once. With no library matching kind, this falls back to RefreshLibrary.
func (c *JellyfinClient) RefreshKind(ctx context.Context, kind string) error {
	if c.URL == "" || c.Token == "" {
		return nil
	}
	id, err := c.libraryFor(ctx, kind)
	if err != nil || id == "" {
		return c.RefreshLibrary(ctx, 0)
	}
	q := url.Values{
		"Recursive":           {"true"},
		"ImageRefreshMode":    {"Default"},
		"MetadataRefreshMode": {"Default"},
		"ReplaceAllImages":    {"false"},
		"RegenerateTrickplay": {"false"},
		"ReplaceAllMetadata":  {"false"},
	}
	return c.post(ctx, "/Items/"+url.PathEscape(id)+"/Refresh?"+q.Encode())
}

// libraryFor returns the id of the library with a folder whose last path
// element is kind, or "" when there is none.
func (c *JellyfinClient) libraryFor(ctx context.Context, kind string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/Library/VirtualFolders", nil)
	if err != nil {
		return "", err
	}
	c.authorize(req)
	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("jellyfin libraries: status %d", resp.StatusCode)
	}
	var folders []struct {
		ItemID    string   `json:"ItemId"`
		Locations []string `json:"Locations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&folders); err != nil {
		return "", err
	}
	for _, f := range folders {
		for _, loc := range f.Locations {
			if path.Base(strings.TrimRight(loc, "/")) == kind {
				return f.ItemID, nil
			}
		}
	}
	return "", nil
}

func (c *JellyfinClient) post(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+endpoint, nil)
	if err != nil {
		return err
	}
	c.authorize(req)

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin refresh: status %d", resp.StatusCode)
	}

	return nil
}

// authorize sets the token header. Jellyfin 12.0 stopped parsing X-Emby-Token;
// the MediaBrowser scheme works on 10.x too.
func (c *JellyfinClient) authorize(req *http.Request) {
	req.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", c.Token))
}
