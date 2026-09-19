package mediaserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tiramisu/internal/catalog"
)

// Reporter tells Jellyfin which stubs were written or removed, without ever
// making it refresh a whole library. A library refresh walks every item in it,
// and Jellyfin retries the probe of each file it has never read, so on this
// library one took hours and held Jellyfin's refresh queue the whole time.
//
// Jellyfin waits LibraryMonitorDelay (60 seconds by default) after the last
// report for a folder, then refreshes the nearest item it holds that is still on
// disk. That is the season or show for an episode of a show it lists, which is
// cheap, so those stubs are reported. For a movie it has not listed, a show it
// has not listed, or anything removed, the nearest item is the library itself;
// see flush and forget for what happens instead. This needs no real-time
// monitoring, which never fires on the FUSE mount: the stubs change on the
// physical tree, not through the mount.
type Reporter struct {
	client *JellyfinClient
	root   string
	delay  time.Duration
	logger *log.Logger

	mu      sync.Mutex
	pending map[string]bool
	// New titles waiting to be listed so they can be refreshed; see listThenRefresh.
	awaiting map[string]bool

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

// flush sends the stubs written in the last few seconds. One whose episode, season
// or show Jellyfin already lists is reported, and Jellyfin refreshes that folder.
// For a new movie or show, the library is walked for new and missing entries only,
// with no metadata and no probe (mode None), and the new title alone is refreshed
// once the walk lists it. A removed path is not sent: Track drops its item.
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
	libs, err := r.client.libraries(ctx)
	if err != nil {
		r.logger.Printf("[Jellyfin] WARNING: cannot report %d changed path(s): %v", len(paths), err)
		return
	}
	var known []string
	walk := map[string]bool{}
	for _, p := range paths {
		// A folder arrives with its stubs, and a removal is Track's.
		if info, err := os.Stat(p); err != nil || info.IsDir() {
			continue
		}
		ids, lib := jellyfinIDs(libs, r.root, p)
		if len(ids) == 0 {
			continue
		}
		listed, err := r.client.listed(ctx, ids...)
		if err != nil {
			r.logger.Printf("[Jellyfin] WARNING: cannot report %s: %v", filepath.Base(p), err)
			continue
		}
		if listed {
			known = append(known, p)
			continue
		}
		walk[lib] = true
		r.listThenRefresh(ids[len(ids)-1], filepath.Base(p))
	}
	if len(known) > 0 {
		if err := r.client.reportChanged(ctx, r.root, known); err != nil {
			r.logger.Printf("[Jellyfin] WARNING: cannot report %d changed path(s): %v", len(known), err)
		}
	}
	for lib := range walk {
		if err := r.client.refresh(ctx, lib, "None"); err != nil {
			r.logger.Printf("[Jellyfin] WARNING: cannot list a new title: %v", err)
		}
	}
}

// listThenRefresh waits for the library walk to list a new movie or show, then
// refreshes that title alone: the walk fetched nothing for it.
func (r *Reporter) listThenRefresh(id, name string) {
	r.mu.Lock()
	if r.awaiting[id] {
		r.mu.Unlock()
		return
	}
	if r.awaiting == nil {
		r.awaiting = map[string]bool{}
	}
	r.awaiting[id] = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.awaiting, id)
			r.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), carryLimit+time.Minute)
		defer cancel()
		for deadline := time.Now().Add(carryLimit); ; {
			if ok, err := r.client.listed(ctx, id); err == nil && ok {
				break
			}
			if time.Now().After(deadline) {
				r.logger.Printf("[Jellyfin] WARNING: %s never reached the library", name)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(carryPoll):
			}
		}
		if err := r.client.refresh(ctx, id, "Default"); err != nil {
			r.logger.Printf("[Jellyfin] WARNING: cannot refresh %s: %v", name, err)
		}
	}()
}

// jellyfinItem is one item a path in Tiramisu's tree stands for: its .NET type
// and the path in that tree.
type jellyfinItem struct{ class, path string }

// lineage returns the items p stands for, deepest first: a movie; an episode, its
// season and its show; or a season or show folder and what holds it.
func lineage(root, p string) []jellyfinItem {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if parts[0] == "movies" {
		if len(parts) != 2 {
			return nil
		}
		return []jellyfinItem{{movieClass, p}}
	}
	if len(parts) < 2 || len(parts) > 4 {
		return nil
	}
	classes := []string{seriesClass, seasonClass, episodeClass}
	var out []jellyfinItem
	for depth := len(parts) - 1; depth >= 1; depth-- {
		out = append(out, jellyfinItem{classes[depth-1], filepath.Join(root, filepath.Join(parts[:depth+1]...))})
	}
	return out
}

// jellyfinIDs returns the ids of the items p stands for, deepest first, and the id of
// the library holding them. Nothing when p is in no library.
func jellyfinIDs(libs []jfLibrary, root, p string) (ids []string, lib string) {
	for _, it := range lineage(root, p) {
		for _, l := range libs {
			if on := serverPaths(l.Locations, root, it.path); len(on) > 0 {
				ids = append(ids, itemID(it.class, on[0]))
				lib = l.ItemId
				break
			}
		}
	}
	return ids, lib
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

// ItemPath returns the file behind a Jellyfin item, or "" when Jellyfin holds no
// such item. The Webhook plugin names an item by id and title only, and a title
// can't tell apart the episodes of one show.
func (r *Reporter) ItemPath(ctx context.Context, id string) (string, error) {
	resp, err := r.client.send(ctx, http.MethodGet, "/Items?fields=Path&ids="+url.QueryEscape(id), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Items []struct {
			Path string `json:"Path"`
		} `json:"Items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Items) == 0 {
		return "", err
	}
	return out.Items[0].Path, nil
}

// jfLibrary is one Jellyfin library: its item id and the folders it points at.
type jfLibrary struct {
	ItemId    string   `json:"ItemId"`
	Locations []string `json:"Locations"`
}

func (c *JellyfinClient) libraries(ctx context.Context) ([]jfLibrary, error) {
	resp, err := c.send(ctx, http.MethodGet, "/Library/VirtualFolders", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var libs []jfLibrary
	if err := json.NewDecoder(resp.Body).Decode(&libs); err != nil {
		return nil, err
	}
	return libs, nil
}

// locations lists the folders of every Jellyfin library.
func (c *JellyfinClient) locations(ctx context.Context) ([]string, error) {
	libs, err := c.libraries(ctx)
	if err != nil {
		return nil, err
	}
	var locs []string
	for _, l := range libs {
		locs = append(locs, l.Locations...)
	}
	return locs, nil
}

// listed reports whether Jellyfin holds any of these items. The items are counted,
// not TotalRecordCount: with limit=0 Jellyfin 12.1 counts every id asked for, held or not.
func (c *JellyfinClient) listed(ctx context.Context, ids ...string) (bool, error) {
	_, n, err := c.query(ctx, "/Items?enableImages=false&enableUserData=false&ids="+url.QueryEscape(strings.Join(ids, ",")))
	return n > 0, err
}

// episodes counts the episodes on disk under a season or show.
func (c *JellyfinClient) episodes(ctx context.Context, id string) (int, error) {
	total, _, err := c.query(ctx, "/Items?limit=1&enableImages=false&recursive=true&includeItemTypes=Episode&isMissing=false&parentId="+url.QueryEscape(id))
	return total, err
}

// query returns an item query's TotalRecordCount and how many items it returned.
func (c *JellyfinClient) query(ctx context.Context, endpoint string) (int, int, error) {
	resp, err := c.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	var body struct {
		TotalRecordCount int               `json:"TotalRecordCount"`
		Items            []json.RawMessage `json:"Items"`
	}
	err = json.NewDecoder(resp.Body).Decode(&body)
	return body.TotalRecordCount, len(body.Items), err
}

// refresh queues a refresh of one item. On a library, mode None only walks its
// folders, adding what is new and dropping what is gone; Default fetches what an
// item lacks, which for a new one is everything, the probe included.
func (c *JellyfinClient) refresh(ctx context.Context, id, mode string) error {
	resp, err := c.send(ctx, http.MethodPost,
		"/Items/"+url.PathEscape(id)+"/Refresh?metadataRefreshMode="+mode+"&imageRefreshMode="+mode, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// deleteItem drops an item and everything under it from Jellyfin. Jellyfin also
// deletes the item's path, so only call it once that path is gone.
func (c *JellyfinClient) deleteItem(ctx context.Context, id string) error {
	resp, err := c.send(ctx, http.MethodDelete, "/Items/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
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
