package app

import (
	"testing"
	"time"
)

// The Office S07E06 ended 19 minutes ago while S07E07 of the same pack plays:
// pruning E06 must leave the pack's priority on.
func TestLiveTorrentHashesKeepsPackWithPlayingSibling(t *testing.T) {
	now := time.Now()
	const pack, other = "d6797f54", "0badc0de"
	entries := map[string]*PlaybackState{
		"/tv/S07E06.mkv": {Hash: pack, OpenedAt: now.Add(-19 * time.Minute)},
		"/tv/S07E07.mkv": {Hash: pack, OpenedAt: now.Add(-20 * time.Minute)},
		"/movie.mkv":     {Hash: other, OpenedAt: now.Add(-40 * time.Minute)},
	}
	for p, ps := range entries {
		playbackRegistry.Store(p, ps)
		defer playbackRegistry.Delete(p)
	}
	none := func(string) (time.Time, bool) { return time.Time{}, false }

	live := liveTorrentHashes(map[string]bool{"/tv/S07E07.mkv": true}, now, 15*time.Minute, none)
	if !live[pack] {
		t.Fatal("pack with an open sibling handle must stay live")
	}
	if live[other] {
		t.Fatal("a 40-minute-old entry with no handle is not live")
	}

	// Direct play opens and closes handles per request: recent reads count too.
	recent := func(p string) (time.Time, bool) { return now.Add(-time.Minute), p == "/tv/S07E07.mkv" }
	if !liveTorrentHashes(map[string]bool{}, now, 15*time.Minute, recent)[pack] {
		t.Fatal("pack with a sibling read a minute ago must stay live")
	}
	if liveTorrentHashes(map[string]bool{}, now, 15*time.Minute, none)[pack] {
		t.Fatal("pack with no handle and no recent activity is not live")
	}
}
