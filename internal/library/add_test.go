package library

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

// Jellyfin keeps a show while its folder is on disk, so removing its last episode
// removes the season and show folders too, and reports each. The tree stays.
func TestRemovePrunesTheFoldersItEmpties(t *testing.T) {
	dir := t.TempDir()
	var reported []string
	m := New(Config{TVDir: dir, Registry: mapRegistry{}, GoStorm: &fakeGoStorm{stats: TorrentStats{
		Hash: testHash, FileStats: files("Show.S01E01.mkv", "Show.S02E01.mkv"),
	}}, InvalidatePath: func(p string) { reported = append(reported, p) }})
	if _, err := m.Add(context.Background(), AddRequest{Type: "tv", Hash: testHash, Title: "Show", FirstAirDate: "2020"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	show := filepath.Join(dir, "Show (2020)")

	if _, err := m.Remove(context.Background(), RemoveRequest{Path: filepath.Join(show, "Season.01", "Show_S01E01_f12d8fd2.mkv")}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(show, "Season.01")); !os.IsNotExist(err) {
		t.Error("the emptied season folder is still there")
	}
	if _, err := os.Stat(show); err != nil {
		t.Error("removed the show while it still had a season")
	}

	if _, err := m.Remove(context.Background(), RemoveRequest{Hash: testHash}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(show); !os.IsNotExist(err) {
		t.Error("the emptied show folder is still there")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("removed the tv tree itself")
	}
	if !slices.Contains(reported, filepath.Join(show, "Season.02")) || !slices.Contains(reported, show) {
		t.Errorf("reported %q, want the season and show folders among them", reported)
	}
}

// The Library page prefills tv for any episode, so a show filed only as anime
// stays anime. A release replaced from another tree takes its emptied folders along.
func TestAddKeepsAnAnimeShowInTheAnimeTree(t *testing.T) {
	tv, anime := t.TempDir(), t.TempDir()
	const other = "0123456789abcdef0123456789abcdef01234567"
	gs := &fakeGoStorm{stats: TorrentStats{FileStats: files("Show.S01E01.mkv")}}
	m := New(Config{TVDir: tv, AnimeDir: anime, Registry: mapRegistry{}, GoStorm: gs})
	add := func(kind, title, hash string) string {
		t.Helper()
		gs.stats.Hash = hash
		got, err := m.Add(context.Background(), AddRequest{
			Type: kind, Hash: hash, Title: title, FirstAirDate: "2020", Season: 1, Episode: 1})
		if err != nil {
			t.Fatalf("Add %s %s: %v", kind, title, err)
		}
		return got.Type
	}

	add("anime", "Show", testHash)
	if got := add("tv", "Show", other); got != "anime" {
		t.Errorf("filed as %s, want anime", got)
	}
	if _, err := os.Stat(filepath.Join(tv, "Show (2020)")); !os.IsNotExist(err) {
		t.Error("the tv add made a show folder in the tv tree")
	}

	// Anime asked for by name moves a tv show, and the tv folders go with it.
	add("tv", "Other", testHash)
	add("anime", "Other", other)
	if _, err := os.Stat(filepath.Join(tv, "Other (2020)")); !os.IsNotExist(err) {
		t.Error("the replaced episode left its tv show folder behind")
	}
	if _, err := os.Stat(tv); err != nil {
		t.Error("pruned the tv tree itself")
	}
}
