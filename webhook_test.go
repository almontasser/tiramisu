package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tiramisu/internal/catalog/mediaserver"
)

// Episodes of one pack share the hash suffix, so only the file Jellyfin names may be
// confirmed, and a stop releases it again.
func TestJellyfinWebhookMatchesTheItemsFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items" || r.URL.Query().Get("ids") != "item2" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"Items":[{"Path":"/media/tiramisu/library/tv/Show (2020)/Season.01/Show_S01E02_8e2aba05.mkv"}]}`))
	}))
	defer srv.Close()
	jellyfinReporter = mediaserver.NewReporter("jellyfin", srv.URL, "tok", "/mnt/real", log.New(io.Discard, "", 0))
	defer func() { jellyfinReporter = nil }()

	e1 := "/mnt/tiramisu/tv/Show (2020)/Season.01/Show_S01E01_8e2aba05.mkv"
	e2 := "/mnt/tiramisu/tv/Show (2020)/Season.01/Show_S01E02_8e2aba05.mkv"
	for _, p := range []string{e1, e2} {
		playbackRegistry.Store(p, &PlaybackState{Path: p})
		defer playbackRegistry.Delete(p)
	}

	// The file's own handle is still open when the stop arrives; the file match must
	// be honored anyway, or the pump lives on until the idle timeout.
	h := &MkvHandle{path: e2}
	activeHandles.Store(h, true)
	defer activeHandles.Delete(h)

	send := func(event string) {
		body := `{"event":"` + event + `","itemId":"item2","Metadata":{"title":"Pilot","grandparentTitle":"Show","librarySectionType":"Episode"}}`
		req := httptest.NewRequest(http.MethodPost, "/plex/webhook", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		handlePlexWebhook(httptest.NewRecorder(), req)
	}

	send("PlaybackStart")
	if !isPlaying(e2) || isPlaying(e1) {
		t.Fatalf("after start: E02 playing=%v, E01 playing=%v; want only E02", isPlaying(e2), isPlaying(e1))
	}
	send("PlaybackStop")
	if isPlaying(e2) {
		t.Fatal("E02 still playing after stop")
	}
}
