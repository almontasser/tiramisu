package library

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type fakeCatalog struct {
	shows map[string]CatalogShow
	aired map[int]map[int]int
}

func (f fakeCatalog) FindShow(_ context.Context, title, _ string) (CatalogShow, error) {
	s, ok := f.shows[title]
	if !ok {
		return CatalogShow{}, fmt.Errorf("no TMDB match for %q", title)
	}
	return s, nil
}

func (f fakeCatalog) AiredEpisodes(_ context.Context, id int) (map[int]int, error) {
	return f.aired[id], nil
}

// fileStub writes an episode stub the way addEpisodes names it.
func fileStub(t *testing.T, root, folder string, season, episode int) {
	t.Helper()
	title, _ := TitleFromFolder(folder)
	dir := filepath.Join(root, folder, fmt.Sprintf("Season.%02d", season))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := EpisodeFilename(title, season, episode, "abcdef01")
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findShow(t *testing.T, rep *MissingReport, folder string) MissingShow {
	t.Helper()
	for _, s := range rep.Shows {
		if s.Folder == folder {
			return s
		}
	}
	t.Fatalf("no row for %q in report", folder)
	return MissingShow{}
}

func TestMissingCategorizesSeasons(t *testing.T) {
	tv := t.TempDir()
	const folder = "Show_Name (2020)"
	fileStub(t, tv, folder, 1, 1)
	fileStub(t, tv, folder, 3, 1)
	fileStub(t, tv, folder, 3, 2)

	m := &Manager{cfg: Config{TVDir: tv, Catalog: fakeCatalog{
		shows: map[string]CatalogShow{"Show Name": {ID: 7, Name: "Show Name"}},
		aired: map[int]map[int]int{7: {1: 3, 2: 2, 3: 4}},
	}}}
	rep, err := m.Missing(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	got := findShow(t, rep, folder)
	if got.Aired != 9 || got.Filed != 3 || got.Missing != 6 || got.Mismatch {
		t.Fatalf("aired/filed/missing/mismatch = %d/%d/%d/%v, want 9/3/6/false", got.Aired, got.Filed, got.Missing, got.Mismatch)
	}
	want := []MissingSeason{
		{Season: 1, Expected: 3, Missing: []int{2, 3}, Category: "only_e01"},
		{Season: 2, Expected: 2, Missing: []int{1, 2}, Category: "whole"},
		{Season: 3, Expected: 4, Missing: []int{3, 4}, Category: "partial"},
	}
	if !reflect.DeepEqual(got.Seasons, want) {
		t.Fatalf("seasons = %+v\nwant     %+v", got.Seasons, want)
	}
	if rep.Totals.ByCategory["only_e01"] != 2 || rep.Totals.ByCategory["whole"] != 2 || rep.Totals.ByCategory["partial"] != 2 {
		t.Fatalf("by category = %v", rep.Totals.ByCategory)
	}
}

// A season whose only filed episode is not E01 is a partial season: the
// "only E01" bucket exists to single out one specific migration failure.
func TestMissingOnlyE01NeedsEpisodeOne(t *testing.T) {
	tv := t.TempDir()
	fileStub(t, tv, "Other (2011)", 1, 5)
	m := &Manager{cfg: Config{TVDir: tv, Catalog: fakeCatalog{
		shows: map[string]CatalogShow{"Other": {ID: 1}},
		aired: map[int]map[int]int{1: {1: 6}},
	}}}
	rep, err := m.Missing(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if c := findShow(t, rep, "Other (2011)").Seasons[0].Category; c != "partial" {
		t.Fatalf("category = %q, want partial", c)
	}
}

func TestMissingFlagsNumberingMismatch(t *testing.T) {
	tv := t.TempDir()
	for e := 1; e <= 6; e++ {
		fileStub(t, tv, "Packed (2010)", 1, e)
	}
	m := &Manager{cfg: Config{TVDir: tv, Catalog: fakeCatalog{
		shows: map[string]CatalogShow{"Packed": {ID: 2}},
		aired: map[int]map[int]int{2: {1: 3, 2: 4}},
	}}}
	rep, err := m.Missing(context.Background(), "tv")
	if err != nil {
		t.Fatal(err)
	}
	got := findShow(t, rep, "Packed (2010)")
	if !got.Mismatch || got.Outside != 3 {
		t.Fatalf("mismatch/outside = %v/%d, want true/3", got.Mismatch, got.Outside)
	}
	if rep.Totals.Mismatch != 1 || len(rep.Totals.ByCategory) != 0 {
		t.Fatalf("a mismatched show must stay out of the category totals: %+v", rep.Totals)
	}
}

func TestMissingReportsUnmatchedShow(t *testing.T) {
	tv := t.TempDir()
	fileStub(t, tv, "Unknown_Show", 1, 1)
	m := &Manager{cfg: Config{TVDir: tv, Catalog: fakeCatalog{}}}
	rep, err := m.Missing(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	got := findShow(t, rep, "Unknown_Show")
	if got.Error == "" || got.Filed != 1 || got.Year != "" {
		t.Fatalf("unmatched show = %+v, want an error, 1 filed, no year", got)
	}
}

func TestMissingFiltersByTree(t *testing.T) {
	tv, anime := t.TempDir(), t.TempDir()
	fileStub(t, tv, "Series (2001)", 1, 1)
	fileStub(t, anime, "Anime (2002)", 1, 1)
	m := &Manager{cfg: Config{TVDir: tv, AnimeDir: anime, Catalog: fakeCatalog{
		shows: map[string]CatalogShow{"Series": {ID: 3}, "Anime": {ID: 4}},
		aired: map[int]map[int]int{3: {1: 1}, 4: {1: 1}},
	}}}
	rep, err := m.Missing(context.Background(), "anime")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Shows) != 1 || rep.Shows[0].Tree != "anime" {
		t.Fatalf("anime filter returned %+v", rep.Shows)
	}
}

func TestMissingNeedsCatalog(t *testing.T) {
	m := &Manager{cfg: Config{TVDir: t.TempDir()}}
	if _, err := m.Missing(context.Background(), "all"); err == nil {
		t.Fatal("expected an error without a catalogue")
	}
}

func TestTitleFromFolderRoundTrips(t *testing.T) {
	for _, tc := range []struct{ folder, title, year string }{
		{"Love,_Death_Robots (2019)", "Love, Death Robots", "2019"},
		{"The_Handmaids_Tale (2017)", "The Handmaids Tale", "2017"},
		{"Prisoner", "Prisoner", ""},
	} {
		title, year := TitleFromFolder(tc.folder)
		if title != tc.title || year != tc.year {
			t.Errorf("TitleFromFolder(%q) = %q, %q; want %q, %q", tc.folder, title, year, tc.title, tc.year)
		}
		if got := ShowFolderName(title, year); got != tc.folder {
			t.Errorf("ShowFolderName(%q, %q) = %q; want the original folder %q", title, year, got, tc.folder)
		}
	}
}

func TestAiredFromSeasons(t *testing.T) {
	seasons := map[int]int{0: 5, 1: 10, 2: 8, 3: 10}
	if got := AiredFromSeasons(seasons, 2, 6); !reflect.DeepEqual(got, map[int]int{1: 10, 2: 6}) {
		t.Errorf("mid-season = %v", got)
	}
	if got := AiredFromSeasons(seasons, 0, 0); !reflect.DeepEqual(got, map[int]int{1: 10, 2: 8, 3: 10}) {
		t.Errorf("no last-aired episode = %v", got)
	}
}
