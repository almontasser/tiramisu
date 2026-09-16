package mediaserver

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCarryMovesWatchStateOntoTheReplacementStub(t *testing.T) {
	const root = "/mnt/real"
	oldID := itemID("MediaBrowser.Controller.Entities.TV.Episode", "/media/library/tv/Show (2020)/Season.01/Show_S01E01_aaaaaaaa.mkv")
	newID := itemID("MediaBrowser.Controller.Entities.TV.Episode", "/media/library/tv/Show (2020)/Season.01/Show_S01E01_bbbbbbbb.mkv")

	var appeared atomic.Bool
	written := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Library/VirtualFolders":
			w.Write([]byte(`[{"Locations":["/media/library/tv"]}]`))
		case r.URL.Path == "/Users":
			w.Write([]byte(`[{"Id":"u1"},{"Id":"u2"}]`))
		case r.URL.Path == "/Items" && r.URL.Query().Get("ids") == oldID:
			if r.URL.Query().Get("userId") == "u1" {
				w.Write([]byte(`{"Items":[{"UserData":{"Played":true,"PlayCount":2,"PlaybackPositionTicks":99,"IsFavorite":true}}]}`))
				return
			}
			w.Write([]byte(`{"Items":[{"UserData":{"Played":false,"PlayCount":0,"PlaybackPositionTicks":0}}]}`))
		case r.URL.Path == "/Items" && r.URL.Query().Get("ids") == newID:
			if !appeared.Load() {
				w.Write([]byte(`{"Items":[]}`)) // not refreshed into the library yet
				return
			}
			w.Write([]byte(`{"Items":[{"UserData":{}}]}`))
		case r.URL.Path == "/Items":
			w.Write([]byte(`{"Items":[]}`)) // a movie id: this stub is an episode
		case strings.HasPrefix(r.URL.Path, "/UserItems/") && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			written <- r.URL.Path + "|" + r.URL.Query().Get("userId") + "|" + string(body)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldPoll := carryPoll
	carryPoll = 5 * time.Millisecond
	defer func() { carryPoll = oldPoll }()

	r := NewReporter("jellyfin", srv.URL, "tok", root, log.New(io.Discard, "", 0))
	r.Carry(root+"/tv/Show (2020)/Season.01/Show_S01E01_aaaaaaaa.mkv",
		root+"/tv/Show (2020)/Season.01/Show_S01E01_bbbbbbbb.mkv")

	select {
	case got := <-written:
		t.Fatalf("wrote %q before the replacement item existed", got)
	case <-time.After(50 * time.Millisecond):
	}
	appeared.Store(true)

	select {
	case got := <-written:
		parts := strings.SplitN(got, "|", 3)
		if parts[0] != "/UserItems/"+newID+"/UserData" || parts[1] != "u1" {
			t.Fatalf("wrote to %s for user %s, want the new item for u1", parts[0], parts[1])
		}
		var d itemUserData
		if err := json.Unmarshal([]byte(parts[2]), &d); err != nil {
			t.Fatalf("body %q: %v", parts[2], err)
		}
		if !d.Played || d.PlayCount != 2 || d.PlaybackPositionTicks != 99 || !d.IsFavorite {
			t.Fatalf("carried %+v, want the watched state", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was carried onto the replacement")
	}

	select {
	case got := <-written:
		t.Fatalf("carried state for a user who had none: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
}
