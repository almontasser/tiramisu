package mediaserver

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"time"
	"unicode/utf16"
)

// The .NET types Jellyfin hashes with a path to make an item id, for the two kinds of
// stub Tiramisu writes.
var itemClasses = []string{
	"MediaBrowser.Controller.Entities.Movies.Movie",
	"MediaBrowser.Controller.Entities.TV.Episode",
}

// carryPoll is how often Carry looks for the replacement item, and carryLimit how long
// it keeps looking. The item exists only once Jellyfin has refreshed the folder, which
// is LibraryMonitorDelay after the report at best, and longer when its refresh queue is
// working through something slow. Vars so the test does not wait them out.
var (
	carryPoll  = 30 * time.Second
	carryLimit = 30 * time.Minute
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
// 14,000-item library to find one path, and works for an item that no longer exists.
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

// Carry moves the watch state of a replaced stub onto the stub that replaced it.
//
// Jellyfin keeps user data per item, and an item is its path: the id is a hash of the
// path, and the UserData rows hang off that id. A replacement release carries a
// different info hash, which is part of the file name, so the new stub is a new item
// that starts unplayed while played, the resume position, the play count and the
// favourite flag stay on the item that just disappeared. Measured on 2026-09-16:
// re-filing 12 Angry Men lost all of it, and restoring the old path brought it back.
// The provider ids in UserData.CustomDataKey do not pair the two items; nothing does.
//
// The old item's state is read at once, while Jellyfin still has it, and written when
// the replacement appears.
//
// ponytail: state lives in this goroutine, so a restart during the wait drops it and
// the replacement stays unplayed. Persist the pending carries if that starts to matter.
func (r *Reporter) Carry(oldPath, newPath string) {
	if r == nil || oldPath == "" || newPath == "" || oldPath == newPath {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), carryLimit+time.Minute)
		defer cancel()
		if err := r.carry(ctx, oldPath, newPath); err != nil {
			r.logger.Printf("[Jellyfin] WARNING: cannot carry the watch state of %s: %v",
				filepath.Base(oldPath), err)
		}
	}()
}

func (r *Reporter) carry(ctx context.Context, oldPath, newPath string) error {
	locations, err := r.client.locations(ctx)
	if err != nil {
		return err
	}
	from := serverPaths(locations, r.root, oldPath)
	to := serverPaths(locations, r.root, newPath)
	if len(from) == 0 || len(to) == 0 {
		return nil // outside every library this server holds
	}

	users, err := r.client.users(ctx)
	if err != nil {
		return err
	}
	class, state := "", map[string]itemUserData{}
	for _, c := range itemClasses {
		id := itemID(c, from[0])
		for _, u := range users {
			d, ok, err := r.client.userData(ctx, id, u)
			if err != nil {
				return err
			}
			if !ok {
				break // not an item of this kind; try the next class
			}
			class = c
			if d.worthCarrying() {
				state[u] = d
			}
		}
		if class != "" {
			break
		}
	}
	if len(state) == 0 {
		return nil // nobody had watched it, or the item was already gone
	}

	newID := itemID(class, to[0])
	for deadline := time.Now().Add(carryLimit); ; {
		if _, ok, err := r.client.userData(ctx, newID, users[0]); err == nil && ok {
			break
		} else if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s never appeared in the library", filepath.Base(newPath))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(carryPoll):
		}
	}

	for user, d := range state {
		if err := r.client.setUserData(ctx, newID, user, d); err != nil {
			return err
		}
	}
	r.logger.Printf("[Jellyfin] carried the watch state of %d user(s) from %s to %s",
		len(state), filepath.Base(oldPath), filepath.Base(newPath))
	return nil
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
