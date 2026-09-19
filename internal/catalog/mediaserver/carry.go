package mediaserver

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
)

// The .NET types Jellyfin hashes with a path to make an item id, for the two kinds of
// stub Tiramisu writes and the folders that hold episodes.
const (
	movieClass   = "MediaBrowser.Controller.Entities.Movies.Movie"
	episodeClass = "MediaBrowser.Controller.Entities.TV.Episode"
	seasonClass  = "MediaBrowser.Controller.Entities.TV.Season"
	seriesClass  = "MediaBrowser.Controller.Entities.TV.Series"
)

// carryPoll is how often a carry looks for the replacement item, carryLimit how long it
// keeps looking, and trackQueue how many stub changes may wait to be examined. The item
// exists only once Jellyfin has refreshed the folder, which is LibraryMonitorDelay after
// the report at best and longer when its refresh queue is busy. Vars so tests need not
// wait them out.
var (
	carryPoll  = 30 * time.Second
	carryLimit = 30 * time.Minute
	// A show removed whole is one change per episode, and ONE PIECE has over 1,100.
	trackQueue = 1 << 14
)

// An episode stub is Show_S01E02_1a2b3c4d.mkv, a movie stub Title_1994_1080p_5.1_hash.mkv.
var (
	reEpisodeStub = regexp.MustCompile(`(?i)_(s\d{1,2}e\d{1,4})_[0-9a-f]{8}\.mkv$`)
	reMovieStub   = regexp.MustCompile(`^(.+_(?:19|20)\d{2})_.*\.mkv$`)
	reEpisodeID   = regexp.MustCompile(`(?i)^s\d{1,2}e\d{1,4}$`)
)

// itemUserData is the part of Jellyfin's user data worth moving with a stub.
type itemUserData struct {
	Played                bool   `json:"Played"`
	PlayCount             int    `json:"PlayCount"`
	PlaybackPositionTicks int64  `json:"PlaybackPositionTicks"`
	IsFavorite            bool   `json:"IsFavorite"`
	LastPlayedDate        string `json:"LastPlayedDate,omitempty"`
}

// worthCarrying reports whether this user has anything to lose.
func (d itemUserData) worthCarrying() bool {
	return d.Played || d.PlayCount > 0 || d.PlaybackPositionTicks > 0 || d.IsFavorite
}

// itemID is Jellyfin's id for a path: the MD5 of the .NET type name and the path in
// UTF-16, read back as a Guid, whose first three fields are little-endian. Checked
// against every movie and episode sampled on 2026-09-16. Computing it saves listing a
// 14,000-item library to find one path, and works for an item already dropped.
func itemID(class, serverPath string) string {
	sum := md5.Sum(utf16LE(class + serverPath))
	b := []byte{sum[3], sum[2], sum[1], sum[0], sum[5], sum[4], sum[7], sum[6]}
	return hex.EncodeToString(append(b, sum[8:]...))
}

func utf16LE(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// identity names what a stub stands for, whatever release is behind it: an episode by
// its show folder and numbers, a film by its title and year. Everything else in a stub's
// name comes from the release - the info hash always, the resolution and audio tags for
// a film - so none of it can be part of the identity. Empty when the path is not a stub.
func identity(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	rel = filepath.ToSlash(rel)
	tree, rest, ok := strings.Cut(rel, "/")
	if !ok {
		return ""
	}
	base := filepath.Base(rest)
	if m := reEpisodeStub.FindStringSubmatch(base); m != nil {
		show, _, ok := strings.Cut(rest, "/")
		if !ok {
			return ""
		}
		return tree + "/" + show + "|" + strings.ToUpper(m[1])
	}
	if m := reMovieStub.FindStringSubmatch(base); m != nil {
		return tree + "|" + m[1]
	}
	return ""
}

func classFor(id string) string {
	_, key, _ := strings.Cut(id, "|")
	if reEpisodeID.MatchString(key) {
		return episodeClass
	}
	return movieClass
}

// UsePendingStore keeps the watch state of removed stubs in a file, so a restart between
// a removal and the release that replaces it does not lose them.
//
// Held state is re-examined here rather than only when the next stub is written: the
// replacement may have been filed while Tiramisu was down, and a wait that ran past
// carryLimit - Jellyfin's refresh queue can sit on a folder for hours - leaves an entry
// nothing else would ever look at again.
func (r *Reporter) UsePendingStore(path string) {
	if r == nil {
		return
	}
	r.store = newPendingStore(path)
	for id, held := range r.store.all() {
		if replacement := r.sibling(held.Path, id); replacement != "" {
			go r.apply(id, held.Users, replacement)
		}
	}
}

// Track follows one stub through a change of release.
//
// Jellyfin keeps user data per item, and an item is its path: the id is a hash of the
// path, and the UserData rows hang off that id. A replacement release carries a
// different info hash, which is part of the file name, so the new stub is a new item
// that starts unplayed while played, the play count, the resume position and the
// favourite flag stay on the item that just disappeared. Measured on 2026-09-16:
// re-filing 12 Angry Men lost all of it, and restoring the old path brought it back.
// The provider ids in UserData.CustomDataKey label a row; they pair nothing.
//
// Every removal reaches this, from the Library API, both sync engines and the reaper, so
// one rule covers each order in which a replacement happens. A removed stub's state is
// read while Jellyfin still has the item, then either carried onto the replacement
// already on disk, or held for the one that arrives later - the reaper drops a dead
// release and the re-search that refills it can be a run or a day later, which is why
// the list is on disk and survives a restart.
func (r *Reporter) Track(path string) {
	if r == nil || path == "" {
		return
	}
	r.trackOnce.Do(func() {
		r.track = make(chan string, trackQueue)
		go r.trackLoop()
	})
	select {
	case r.track <- path:
	default:
		r.logger.Printf("[Jellyfin] WARNING: too many stub changes at once, not tracking %s",
			filepath.Base(path))
	}
}

func (r *Reporter) trackLoop() {
	for p := range r.track {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, statErr := os.Stat(p)
		var err error
		replaced := false
		if id := identity(r.root, p); id != "" {
			if statErr == nil {
				err = r.written(ctx, id, p)
			} else {
				err = r.removed(ctx, id, p)
				replaced = r.sibling(p, id) != ""
			}
		}
		// Only after the watch state is read: dropping the item first would lose it. A
		// stub another release replaced is left to Jellyfin, whose refresh that lists the
		// new stub drops the old one in the same pass. Dropping it here left 240 episodes
		// of Bleach missing for as long as a pack swap took.
		if err == nil && os.IsNotExist(statErr) && !replaced {
			err = r.forget(ctx, p)
		}
		cancel()
		if err != nil {
			r.logger.Printf("[Jellyfin] WARNING: cannot track %s: %v", filepath.Base(p), err)
		}
	}
}

// forget drops a removed stub or folder from Jellyfin: its own item only. Reporting
// the path made Jellyfin refresh the nearest item still on disk, which for a movie, or
// a show removed whole, is the whole library.
//
// A season or show goes on its folder's own report. A folder is removed only once it
// is empty, so that report queues here behind every stub in it, and each episode's
// watch state is read before the folder's item, which takes its episodes along, is
// dropped. Counting the episodes left instead doesn't work: Jellyfin groups shows by
// TMDB id, so a show in two folders counts the other folder's episodes as its own.
func (r *Reporter) forget(ctx context.Context, p string) error {
	libs, err := r.client.libraries(ctx)
	if err != nil {
		return err
	}
	ids, _ := jellyfinIDs(libs, r.root, p)
	if len(ids) == 0 {
		return nil
	}
	listed, err := r.client.listed(ctx, ids[0])
	if err != nil || !listed {
		return err
	}
	return r.client.deleteItem(ctx, ids[0])
}

// removed reads what the users had on a stub that has just gone, and either carries it
// onto the replacement already written or holds it for the one still to come.
func (r *Reporter) removed(ctx context.Context, id, p string) error {
	state, err := r.capture(ctx, classFor(id), p)
	if err != nil || len(state) == 0 {
		return err
	}
	if sibling := r.sibling(p, id); sibling != "" {
		go r.apply(id, state, sibling)
		return nil
	}
	r.store.put(id, pending{Path: p, Users: state, Saved: time.Now()})
	r.logger.Printf("[Jellyfin] holding the watch state of %d user(s) for %s until a release replaces it",
		len(state), filepath.Base(p))
	return nil
}

// written carries the state a removed stub left behind onto the stub replacing it.
func (r *Reporter) written(ctx context.Context, id, p string) error {
	held, ok := r.store.take(id)
	if !ok {
		return nil
	}
	if held.Path == p {
		r.store.drop(id) // same path again: the item id never changed
		return nil
	}
	go r.apply(id, held.Users, p)
	return nil
}

// capture reads every user's state for the stub at p, keeping what is worth moving.
func (r *Reporter) capture(ctx context.Context, class, p string) (map[string]itemUserData, error) {
	locations, err := r.client.locations(ctx)
	if err != nil {
		return nil, err
	}
	on := serverPaths(locations, r.root, p)
	if len(on) == 0 {
		return nil, nil // outside every library this server holds
	}
	users, err := r.client.users(ctx)
	if err != nil {
		return nil, err
	}
	id := itemID(class, on[0])
	state := map[string]itemUserData{}
	for _, u := range users {
		d, ok, err := r.client.userData(ctx, id, u)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil // Jellyfin never held this stub
		}
		if d.worthCarrying() {
			state[u] = d
		}
	}
	return state, nil
}

// apply waits for the replacement to reach the library and writes the state onto it.
func (r *Reporter) apply(id string, state map[string]itemUserData, newPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), carryLimit+time.Minute)
	defer cancel()

	locations, err := r.client.locations(ctx)
	if err != nil {
		r.logger.Printf("[Jellyfin] WARNING: cannot carry the watch state onto %s: %v", filepath.Base(newPath), err)
		return
	}
	on := serverPaths(locations, r.root, newPath)
	users, err := r.client.users(ctx)
	if err != nil || len(on) == 0 || len(users) == 0 {
		return
	}
	newID := itemID(classFor(id), on[0])

	for deadline := time.Now().Add(carryLimit); ; {
		if _, ok, err := r.client.userData(ctx, newID, users[0]); err == nil && ok {
			break
		}
		if time.Now().After(deadline) {
			r.logger.Printf("[Jellyfin] WARNING: %s never reached the library, its watch state is still held",
				filepath.Base(newPath))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(carryPoll):
		}
	}

	for user, d := range state {
		if err := r.client.setUserData(ctx, newID, user, d); err != nil {
			r.logger.Printf("[Jellyfin] WARNING: cannot carry the watch state onto %s: %v", filepath.Base(newPath), err)
			return
		}
	}
	r.store.drop(id)
	r.logger.Printf("[Jellyfin] carried the watch state of %d user(s) onto %s", len(state), filepath.Base(newPath))
}

// sibling returns the stub that already stands for the same episode or film, when a
// replacement was written before the old one was removed. Empty when there is none.
func (r *Reporter) sibling(p, id string) string {
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		other := filepath.Join(filepath.Dir(p), e.Name())
		if other != p && !e.IsDir() && identity(r.root, other) == id {
			return other
		}
	}
	return ""
}

// users lists every user id on the server.
func (c *JellyfinClient) users(ctx context.Context) ([]string, error) {
	resp, err := c.send(ctx, http.MethodGet, "/Users", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var users []struct{ Id string }
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Id)
	}
	return out, nil
}

// userData returns one user's state for an item, and whether the item exists at all.
func (c *JellyfinClient) userData(ctx context.Context, id, user string) (itemUserData, bool, error) {
	endpoint := fmt.Sprintf("/Items?ids=%s&userId=%s&fields=UserData&limit=1",
		url.QueryEscape(id), url.QueryEscape(user))
	resp, err := c.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return itemUserData{}, false, err
	}
	defer resp.Body.Close()
	var body struct {
		Items []struct {
			UserData itemUserData `json:"UserData"`
		} `json:"Items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return itemUserData{}, false, err
	}
	if len(body.Items) == 0 {
		return itemUserData{}, false, nil
	}
	return body.Items[0].UserData, true, nil
}

// setUserData writes one user's state onto an item.
func (c *JellyfinClient) setUserData(ctx context.Context, id, user string, d itemUserData) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("/UserItems/%s/UserData?userId=%s", url.QueryEscape(id), url.QueryEscape(user))
	resp, err := c.send(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
