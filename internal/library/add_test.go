package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"tiramisu/internal/metadb"
)

type mapRegistry map[string]metadb.EpisodeEntry

func (r mapRegistry) UpsertEpisode(key string, e metadb.EpisodeEntry) error { r[key] = e; return nil }
func (r mapRegistry) DeleteEpisode(key string) error                        { delete(r, key); return nil }
func (r mapRegistry) EpisodesByFilePath(string) ([]metadb.EpisodeEntry, error) {
	return nil, nil
}
func (r mapRegistry) GetEpisode(key string) (*metadb.EpisodeEntry, bool, error) {
	e, ok := r[key]
	return &e, ok, nil
}

// A multi-season pack has no one season to give, and the add refused it
// without one. Each file names its own season.
func TestAddMultiSeasonPackNeedsNoSeason(t *testing.T) {
	dir := t.TempDir()
	m := New(Config{TVDir: dir, Registry: mapRegistry{}, GoStorm: &fakeGoStorm{stats: TorrentStats{
		Hash: testHash, Title: "Show.S01-S02.1080p",
		FileStats: files("S01/Show.S01E01.mkv", "S01/Show.S01E02.mkv", "S02/Show.S02E01.mkv"),
	}}})

	got, err := m.Add(context.Background(), AddRequest{Type: "tv", Hash: testHash, Title: "Show", FirstAirDate: "2020"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(got.Files) != 3 {
		t.Fatalf("filed %d episodes, want 3", len(got.Files))
	}
	for _, p := range []string{"Season.01/Show_S01E01_f12d8fd2.mkv", "Season.02/Show_S02E01_f12d8fd2.mkv"} {
		if _, err := os.Stat(filepath.Join(dir, "Show (2020)", p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}

	if _, err := m.Add(context.Background(), AddRequest{Type: "tv", Hash: testHash, Title: "Show", Episode: 1}); err == nil {
		t.Error("a single episode was filed with no season")
	}
}
