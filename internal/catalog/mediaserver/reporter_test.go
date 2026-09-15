package mediaserver

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestReporterMovesPathsOntoLibraryFolders(t *testing.T) {
	got := make(chan []string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != `MediaBrowser Token="tok"` {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /Library/VirtualFolders":
			w.Write([]byte(`[{"Locations":["/media/tiramisu/library/movies"]},{"Locations":["/media/tiramisu/library/tv/"]}]`))
		case "POST /Library/Media/Updated":
			var body struct{ Updates []struct{ Path string } }
			json.NewDecoder(r.Body).Decode(&body)
			var paths []string
			for _, u := range body.Updates {
				paths = append(paths, u.Path)
			}
			got <- paths
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	r := NewReporter("jellyfin", srv.URL, "tok", "/mnt/real", log.New(io.Discard, "", 0))
	r.delay = 10 * time.Millisecond
	r.Changed("/mnt/real/tv/Show (2020)/Season.01/Show_S01E01.mkv")
	r.Changed("/mnt/real/movies/Film_2020_1080p.mkv")
	r.Changed("/mnt/real/tv/Show (2020)/Season.01/Show_S01E01.mkv") // reported once
	r.Changed("/mnt/real/anime/Other/Other_S01E01.mkv")             // no anime library
	r.Changed("/mnt/real-other/tv/x.mkv")                           // shares only a prefix with root
	r.Changed("/elsewhere/x.mkv")

	want := []string{
		"/media/tiramisu/library/movies/Film_2020_1080p.mkv",
		"/media/tiramisu/library/tv/Show (2020)/Season.01/Show_S01E01.mkv",
	}
	select {
	case paths := <-got:
		if !reflect.DeepEqual(paths, want) {
			t.Fatalf("reported %q, want %q", paths, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no report reached Jellyfin")
	}

	if NewReporter("plex", srv.URL, "tok", "/mnt/real", nil) != nil {
		t.Fatal("a Plex setup got a Jellyfin reporter")
	}
	var none *Reporter
	none.Changed("/mnt/real/movies/Film_2020_1080p.mkv") // must not panic
}
