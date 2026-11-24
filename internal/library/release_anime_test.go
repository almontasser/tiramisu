package library

import (
	"context"
	"testing"
)

// Upstream's release point lists movie and tv stubs only; the fork's anime tree holds
// torrents too. A merge that drops "anime" from that list releases a torrent still
// behind an anime episode.
func TestReleaseIfUnusedKeepsAnAnimeStubsTorrent(t *testing.T) {
	gs := &fakeGoStorm{stats: TorrentStats{Hash: testHash, FileStats: files("Show.S01E01.mkv")}}
	m := New(Config{TVDir: t.TempDir(), AnimeDir: t.TempDir(), Registry: mapRegistry{}, GoStorm: gs})
	if _, err := m.Add(context.Background(), AddRequest{
		Type: "anime", Hash: testHash, Title: "Show", FirstAirDate: "2020", Season: 1, Episode: 1}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	released, err := m.ReleaseIfUnused(context.Background(), testHash)
	if err != nil || released || len(gs.removed) > 0 {
		t.Fatalf("released=%v err=%v removed=%v, want the torrent kept", released, err, gs.removed)
	}
}
