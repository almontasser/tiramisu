package mediaserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tiramisu/internal/catalog"
)

// Reporter tells Jellyfin which stubs were written or removed, so that it
// refreshes only the folders that hold them. A library refresh walks every
// item in the library, and Jellyfin retries the probe of each file it has never
// read, so on this library one took hours.
//
// Jellyfin waits LibraryMonitorDelay (60 seconds by default) after the last
// report for a folder, then refreshes the nearest item that still exists: the
// season or show for an episode, but the whole movies folder for a movie,
// because movies sit side by side in one folder. This needs no real-time
// monitoring, which never fires on the FUSE mount: the stubs change on the
// physical tree, not through the mount.
type Reporter struct {
	client *JellyfinClient
	root   string
	delay  time.Duration
	logger *log.Logger

	mu      sync.Mutex
	pending map[string]bool

	// Watch state of stubs a release change removed, and the queue that examines
	// them; see Track. A nil store means nothing is held between restarts.
	store     *pendingStore
	track     chan string
	trackOnce sync.Once
}

// NewReporter returns a Reporter for a Jellyfin server, or nil for any other
// setup; a nil Reporter ignores every report. root is the directory holding the
// movies, tv and anime trees.
func NewReporter(serverType, url, token, root string, logger *log.Logger) *Reporter {
	if serverType != "jellyfin" || url == "" || token == "" {
		return nil
	}
	return &Reporter{
		client: &JellyfinClient{http: catalog.NewClient(15 * time.Second), URL: url, Token: token},
		root:   filepath.Clean(root),
		delay:  5 * time.Second,
		logger: logger,
		store:  newPendingStore(""),
	}
}

// Changed records that the stub or directory at p was written or removed. The
// reports of the next few seconds go to Jellyfin in one request.
func (r *Reporter) Changed(p string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[string]bool{}
		time.AfterFunc(r.delay, r.flush)
	}
	r.pending[p] = true
}

func (r *Reporter) flush() {
	r.mu.Lock()
	paths := make([]string, 0, len(r.pending))
	for p := range r.pending {
		paths = append(paths, p)
	}
	r.pending = nil
	r.mu.Unlock()
	sort.Strings(paths)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// ponytail: a failed report is logged, not retried; Jellyfin's scheduled Scan
	// Media Library picks the change up. Queue and retry if Jellyfin restarts often.
	if err := r.client.reportChanged(ctx, r.root, paths); err != nil {
		r.logger.Printf("[Jellyfin] WARNING: cannot report %d changed path(s): %v", len(paths), err)
	}
}

// reportChanged sends paths to POST /Library/Media/Updated, moved from
// Tiramisu's tree onto the folders Jellyfin's libraries point at: a path under
// root/tv goes under the library location whose last element is tv. A path
// outside root, or under a directory no location is named for, is left out.
func (c *JellyfinClient) reportChanged(ctx context.Context, root string, paths []string) error {
	locations, err := c.locations(ctx)
	if err != nil {
		return err
	}
	type update struct {
		Path string `json:"Path"`
	}
	var updates []update
	for _, p := range paths {
		for _, moved := range serverPaths(locations, root, p) {
			updates = append(updates, update{moved})
		}
	}
	if len(updates) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string][]update{"Updates": updates})
	if err != nil {
		return err
	}
	resp, err := c.send(ctx, http.MethodPost, "/Library/Media/Updated", body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// serverPaths moves a path in Tiramisu's tree onto the folders the media server's
// libraries point at: a path under root/tv goes under every location whose last element
// is tv. Empty when the path sits outside root, or under a directory no location is
// named for.
func serverPaths(locations []string, root, p string) []string {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	top, rest, _ := strings.Cut(filepath.ToSlash(rel), "/")
	var out []string
	for _, loc := range locations {
		loc = strings.TrimRight(loc, "/")
		if path.Base(loc) == top {
			out = append(out, path.Join(loc, rest))
		}
	}
	return out
}

// locations lists the folders of every Jellyfin library.
func (c *JellyfinClient) locations(ctx context.Context) ([]string, error) {
	resp, err := c.send(ctx, http.MethodGet, "/Library/VirtualFolders", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var folders []struct {
		Locations []string `json:"Locations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&folders); err != nil {
		return nil, err
	}
	var locs []string
	for _, f := range folders {
		locs = append(locs, f.Locations...)
	}
	return locs, nil
}

// send makes an authorized request and fails on a status outside 2xx. Jellyfin
// 12.0 stopped parsing X-Emby-Token; the MediaBrowser scheme works on 10.x too.
func (c *JellyfinClient) send(ctx context.Context, method, endpoint string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", c.Token))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("jellyfin %s %s: status %d", method, endpoint, resp.StatusCode)
	}
	return resp, nil
}
