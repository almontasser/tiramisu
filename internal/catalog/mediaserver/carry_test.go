package mediaserver

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJellyfin serves the three calls a carry makes: the library folders, the users, and
// one item's user data. onServer is the item that holds watch state; appeared gates when
// the replacement shows up in the library.
func fakeJellyfin(t *testing.T, watchedID string, appeared *atomic.Bool, replacementID string) (*httptest.Server, chan string) {
	t.Helper()
	written := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Library/VirtualFolders":
			w.Write([]byte(`[{"Locations":["/media/library/tv"]}]`))
		case r.URL.Path == "/Users":
			w.Write([]byte(`[{"Id":"u1"},{"Id":"u2"}]`))
		case r.URL.Path == "/Items" && r.URL.Query().Get("ids") == watchedID:
			if r.URL.Query().Get("userId") == "u1" {
				w.Write([]byte(`{"Items":[{"UserData":{"Played":true,"PlayCount":2,"PlaybackPositionTicks":99,"IsFavorite":true}}]}`))
				return
			}
			w.Write([]byte(`{"Items":[{"UserData":{}}]}`))
		case r.URL.Path == "/Items" && r.URL.Query().Get("ids") == replacementID:
			if appeared.Load() {
				w.Write([]byte(`{"Items":[{"UserData":{}}]}`))
				return
			}
			w.Write([]byte(`{"Items":[]}`))
		case r.URL.Path == "/Items":
			w.Write([]byte(`{"Items":[]}`))
		case strings.HasPrefix(r.URL.Path, "/UserItems/") && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			written <- r.URL.Path + "|" + r.URL.Query().Get("userId") + "|" + string(body)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, written
}

func serverPathOf(name string) string {
	return "/media/library/tv/Show (2020)/Season.01/" + name
}

func waitForCarry(t *testing.T, written chan string, wantID string) itemUserData {
	t.Helper()
	select {
	case got := <-written:
		parts := strings.SplitN(got, "|", 3)
		if parts[0] != "/UserItems/"+wantID+"/UserData" || parts[1] != "u1" {
			t.Fatalf("wrote to %s for %s, want the replacement for u1", parts[0], parts[1])
		}
		var d itemUserData
		if err := json.Unmarshal([]byte(parts[2]), &d); err != nil {
			t.Fatalf("body %q: %v", parts[2], err)
		}
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was carried onto the replacement")
		return itemUserData{}
	}
}

func TestCarryHoldsWatchStateOnDiskUntilTheReplacementArrives(t *testing.T) {
	root := t.TempDir()
	season := filepath.Join(root, "tv", "Show (2020)", "Season.01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(season, "Show_S01E01_aaaaaaaa.mkv")
	newPath := filepath.Join(season, "Show_S01E01_bbbbbbbb.mkv")
	oldID := itemID(episodeClass, serverPathOf("Show_S01E01_aaaaaaaa.mkv"))
	newID := itemID(episodeClass, serverPathOf("Show_S01E01_bbbbbbbb.mkv"))

	var appeared atomic.Bool
	srv, written := fakeJellyfin(t, oldID, &appeared, newID)
	oldPoll := carryPoll
	carryPoll = 5 * time.Millisecond
	defer func() { carryPoll = oldPoll }()

	store := filepath.Join(t.TempDir(), "carry-pending.json")

	// The reaper drops a dead release: nothing replaces it yet, so the state is held.
	before := NewReporter("jellyfin", srv.URL, "tok", root, log.New(io.Discard, "", 0))
	before.UsePendingStore(store)
	before.Track(oldPath)
	for deadline := time.Now().Add(5 * time.Second); ; {
		if data, err := os.ReadFile(store); err == nil && strings.Contains(string(data), "S01E01") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watch state was never written to the pending list")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Tiramisu restarts, and the re-search files a different release for that episode.
	after := NewReporter("jellyfin", srv.URL, "tok", root, log.New(io.Discard, "", 0))
	after.UsePendingStore(store)
	if err := os.WriteFile(newPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	after.Track(newPath)

	select {
	case got := <-written:
		t.Fatalf("wrote %q before the replacement reached the library", got)
	case <-time.After(50 * time.Millisecond):
	}
	appeared.Store(true)

	d := waitForCarry(t, written, newID)
	if !d.Played || d.PlayCount != 2 || d.PlaybackPositionTicks != 99 || !d.IsFavorite {
		t.Fatalf("carried %+v, want the watched state", d)
	}
	select {
	case got := <-written:
		t.Fatalf("carried state for a user who had none: %q", got)
	case <-time.After(50 * time.Millisecond):
	}

	for deadline := time.Now().Add(5 * time.Second); ; {
		data, err := os.ReadFile(store)
		if err == nil && !strings.Contains(string(data), "S01E01") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pending list still holds the episode: %s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCarryFindsAReplacementWrittenBeforeTheRemoval(t *testing.T) {
	root := t.TempDir()
	season := filepath.Join(root, "tv", "Show (2020)", "Season.01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(season, "Show_S01E01_aaaaaaaa.mkv")
	newPath := filepath.Join(season, "Show_S01E01_bbbbbbbb.mkv")
	if err := os.WriteFile(newPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldID := itemID(episodeClass, serverPathOf("Show_S01E01_aaaaaaaa.mkv"))
	newID := itemID(episodeClass, serverPathOf("Show_S01E01_bbbbbbbb.mkv"))

	var appeared atomic.Bool
	appeared.Store(true)
	srv, written := fakeJellyfin(t, oldID, &appeared, newID)

	r := NewReporter("jellyfin", srv.URL, "tok", root, log.New(io.Discard, "", 0))
	r.UsePendingStore(filepath.Join(t.TempDir(), "carry-pending.json"))
	r.Track(oldPath) // the pack add wrote the new stub first, then deleted this one

	if d := waitForCarry(t, written, newID); !d.Played || d.PlayCount != 2 {
		t.Fatalf("carried %+v, want the watched state", d)
	}
}

func TestIdentityIgnoresTheRelease(t *testing.T) {
	root := "/mnt/real"
	cases := []struct{ path, want string }{
		{root + "/tv/Show (2020)/Season.01/Show_S01E01_aaaaaaaa.mkv", "tv/Show (2020)|S01E01"},
		{root + "/anime/ONE_PIECE (1999)/Season.07/ONE_PIECE_S07E222_3e321958.mkv", "anime/ONE_PIECE (1999)|S07E222"},
		{root + "/movies/12_Angry_Men_1957_1080p_5.1_c52440b1.mkv", "movies|12_Angry_Men_1957"},
		{root + "/movies/12_Angry_Men_1957_1080p_de489883.mkv", "movies|12_Angry_Men_1957"},
		{root + "/movies/not-a-stub.txt", ""},
	}
	for _, c := range cases {
		if got := identity(root, c.path); got != c.want {
			t.Errorf("identity(%s) = %q, want %q", filepath.Base(c.path), got, c.want)
		}
	}
	if classFor("tv/Show (2020)|S01E01") != episodeClass || classFor("movies|12_Angry_Men_1957") != movieClass {
		t.Error("classFor picked the wrong Jellyfin type")
	}
}

func TestCarryRetriesHeldStateWhenTiramisuStarts(t *testing.T) {
	root := t.TempDir()
	season := filepath.Join(root, "tv", "Show (2020)", "Season.01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(season, "Show_S01E01_aaaaaaaa.mkv")
	newPath := filepath.Join(season, "Show_S01E01_bbbbbbbb.mkv")
	oldID := itemID(episodeClass, serverPathOf("Show_S01E01_aaaaaaaa.mkv"))
	newID := itemID(episodeClass, serverPathOf("Show_S01E01_bbbbbbbb.mkv"))

	var appeared atomic.Bool
	appeared.Store(true)
	srv, written := fakeJellyfin(t, oldID, &appeared, newID)
	store := filepath.Join(t.TempDir(), "carry-pending.json")

	before := NewReporter("jellyfin", srv.URL, "tok", root, log.New(io.Discard, "", 0))
	before.UsePendingStore(store)
	before.Track(oldPath)
	for deadline := time.Now().Add(5 * time.Second); ; {
		if data, err := os.ReadFile(store); err == nil && strings.Contains(string(data), "S01E01") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watch state was never written to the pending list")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The replacement is filed while Tiramisu is down, so no write ever reaches Track.
	if err := os.WriteFile(newPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := NewReporter("jellyfin", srv.URL, "tok", root, log.New(io.Discard, "", 0))
	after.UsePendingStore(store)

	if d := waitForCarry(t, written, newID); !d.Played || d.PlayCount != 2 {
		t.Fatalf("carried %+v, want the watched state", d)
	}
}
