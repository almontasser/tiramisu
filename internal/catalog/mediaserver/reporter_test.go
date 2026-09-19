package mediaserver

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLibrary is a Jellyfin holding the items in listed. It records every report,
// refresh and delete as one line, in order.
type fakeLibrary struct {
	mu       sync.Mutex
	listed   map[string]bool
	episodes map[string]int // parent id -> episodes left under it
	calls    []string
}

func (f *fakeLibrary) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listed[id]
}

func (f *fakeLibrary) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeLibrary) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLibrary) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != `MediaBrowser Token="tok"` {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/Library/VirtualFolders":
			w.Write([]byte(`[{"ItemId":"movieslib","Locations":["/media/library/movies"]},{"ItemId":"tvlib","Locations":["/media/library/tv/"]}]`))
		case r.URL.Path == "/Users":
			w.Write([]byte(`[{"Id":"u1"}]`))
		case r.URL.Path == "/Items" && q.Get("parentId") != "":
			f.mu.Lock()
			n := f.episodes[q.Get("parentId")]
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]int{"TotalRecordCount": n})
		case r.URL.Path == "/Items":
			items := []map[string]string{}
			for _, id := range strings.Split(q.Get("ids"), ",") {
				if f.has(id) {
					items = append(items, map[string]string{"Id": id})
				}
			}
			// Jellyfin 12.1 answers a limit=0 query by ids with the number asked for.
			json.NewEncoder(w).Encode(map[string]any{"Items": items, "TotalRecordCount": len(strings.Split(q.Get("ids"), ","))})
		case r.Method == http.MethodPost && r.URL.Path == "/Library/Media/Updated":
			var body struct{ Updates []struct{ Path string } }
			json.NewDecoder(r.Body).Decode(&body)
			for _, u := range body.Updates {
				f.record("report " + u.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Refresh"):
			f.record("refresh " + strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/Items/"), "/Refresh") + " " + q.Get("metadataRefreshMode"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/Items/"):
			id := strings.TrimPrefix(r.URL.Path, "/Items/")
			f.mu.Lock()
			delete(f.listed, id)
			f.mu.Unlock()
			f.record("delete " + id)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitCalls(t *testing.T, f *fakeLibrary, n int) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if got := f.seen(); len(got) >= n {
			time.Sleep(50 * time.Millisecond) // anything beyond n is a failure too
			return f.seen()
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %q, want %d calls", f.seen(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Adding a stub must never make Jellyfin refresh a whole library. An episode of a
// show it lists is reported, which refreshes that season or show. A movie it does not
// list is walked in with mode None, which fetches nothing, and then refreshed alone.
func TestReporterNeverRefreshesALibraryForAnAdd(t *testing.T) {
	root := t.TempDir()
	episode := filepath.Join(root, "tv", "Show (2020)", "Season.01", "Show_S01E02_aaaaaaaa.mkv")
	movie := filepath.Join(root, "movies", "Film_2020_1080p_bbbbbbbb.mkv")
	touch(t, episode)
	touch(t, movie)
	season := itemID(seasonClass, "/media/library/tv/Show (2020)/Season.01")
	movieID := itemID(movieClass, "/media/library/movies/Film_2020_1080p_bbbbbbbb.mkv")

	f := &fakeLibrary{listed: map[string]bool{season: true}}
	srv := f.serve(t)
	oldPoll := carryPoll
	carryPoll = 5 * time.Millisecond
	defer func() { carryPoll = oldPoll }()

	r := NewReporter("jellyfin", srv.URL, "tok", root, log.New(io.Discard, "", 0))
	r.delay = 10 * time.Millisecond
	r.Changed(episode)
	r.Changed(movie)
	r.Changed(filepath.Join(root, "movies", "Gone_2020_1080p_cccccccc.mkv")) // removed: Track's
	r.Changed(filepath.Join(root, "tv", "Show (2020)", "Season.01"))         // a folder
	r.Changed("/elsewhere/x.mkv")

	want := []string{
		"report /media/library/tv/Show (2020)/Season.01/Show_S01E02_aaaaaaaa.mkv",
		"refresh movieslib None",
	}
	if got := waitCalls(t, f, 2); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	// The walk lists the movie; only then is it refreshed, and with everything.
	f.mu.Lock()
	f.listed[movieID] = true
	f.mu.Unlock()
	want = append(want, "refresh "+movieID+" Default")
	if got := waitCalls(t, f, 3); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	var none *Reporter
	none.Changed(movie) // must not panic
}

// A show removed whole leaves Jellyfin item by item, once each watch state is read,
// and its season and show go with the last episode, with nothing refreshed.
func TestForgetDropsTheShowWithItsLastEpisode(t *testing.T) {
	root := t.TempDir()
	on := "/media/library/tv/Show (2020)"
	e1 := itemID(episodeClass, on+"/Season.01/Show_S01E01_aaaaaaaa.mkv")
	e2 := itemID(episodeClass, on+"/Season.01/Show_S01E02_aaaaaaaa.mkv")
	season := itemID(seasonClass, on+"/Season.01")
	show := itemID(seriesClass, on)

	f := &fakeLibrary{
		listed:   map[string]bool{e1: true, e2: true, season: true, show: true},
		episodes: map[string]int{season: 1, show: 1}, // E02 is still in Jellyfin
	}
	r := NewReporter("jellyfin", f.serve(t).URL, "tok", root, log.New(io.Discard, "", 0))

	// The stubs and folders are gone from disk; E01 is examined first.
	r.Track(filepath.Join(root, "tv", "Show (2020)", "Season.01", "Show_S01E01_aaaaaaaa.mkv"))
	if got := waitCalls(t, f, 1); !reflect.DeepEqual(got, []string{"delete " + e1}) {
		t.Fatalf("got %q: the season went while an episode was left", got)
	}

	f.mu.Lock()
	f.episodes = map[string]int{}
	f.mu.Unlock()
	r.Track(filepath.Join(root, "tv", "Show (2020)", "Season.01", "Show_S01E02_aaaaaaaa.mkv"))
	want := []string{"delete " + e1, "delete " + e2, "delete " + season, "delete " + show}
	if got := waitCalls(t, f, 4); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	// A season folder still on disk keeps its item, and the show's.
	kept := filepath.Join(root, "tv", "Kept (2020)", "Season.01")
	if err := os.MkdirAll(kept, 0o755); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.listed[itemID(seasonClass, "/media/library/tv/Kept (2020)/Season.01")] = true
	f.mu.Unlock()
	r.Track(filepath.Join(kept, "Kept_S01E01_aaaaaaaa.mkv"))
	if got := waitCalls(t, f, 4); len(got) != 4 {
		t.Fatalf("got %q: dropped an item whose folder is on disk", got)
	}
}
