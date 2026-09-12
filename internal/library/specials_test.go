package library

import "testing"

func TestIsExtrasPath(t *testing.T) {
	for path, want := range map[string]bool{
		"The Last of Us S02/Specials/S00E40 - Joel's Journey.mkv": true,
		"Show/Extras/Show S01E01 Featurette.mkv":                  true,
		"Show/Behind the Scenes/Making Of.mkv":                    true,
		"Show/Season 1/Show S01E01.mkv":                           false,
		"Show S01E01 Special Guest.mkv":                           false,
	} {
		if got := IsExtrasPath(path); got != want {
			t.Errorf("IsExtrasPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// A season 2 pack of The Last of Us carried its podcasts and featurettes as
// Specials/S00E40-E57, and the add filed all 18 as episodes of season 2.
func TestPackEpisodesSkipsBonusMaterial(t *testing.T) {
	files := []FileStat{
		{ID: 1, Path: "TLOU S02/The Last of Us - S02E01 - Future Days.mkv"},
		{ID: 2, Path: "TLOU S02/Specials/S00E40 - Joel's Journey to Season 2.mkv"},
		{ID: 3, Path: "TLOU S02/S00E41 - Inside the Episode S2 E1.mkv"},
		{ID: 4, Path: "TLOU S02/Extras/The Last of Us - S02E01 - Featurette.mkv"},
		{ID: 5, Path: "TLOU S02/The Last of Us - S02E02 - Through the Valley.mkv"},
		{ID: 6, Path: "TLOU S02/The Last of Us - S02E02.nfo"},
	}
	got := packEpisodes(files)
	if len(got) != 2 || got[0].file.ID != 1 || got[1].file.ID != 5 {
		t.Fatalf("got %+v; want only files 1 and 5", got)
	}
	if got[0].season != 2 || got[0].episode != 1 || got[1].episode != 2 {
		t.Fatalf("got %+v; want S02E01 and S02E02", got)
	}
}

// An S00 file is a special, not the same-numbered episode of the season asked for.
func TestPickFileForEpisodeIgnoresSpecials(t *testing.T) {
	m := &Manager{}
	files := []FileStat{
		{ID: 1, Path: "Show/Specials/S00E03 - Behind the Episode.mkv", Length: 900},
		{ID: 2, Path: "Show/Show S02E03.mkv", Length: 800},
	}
	got, err := m.pickFileForEpisode(AddRequest{Season: 2, Episode: 3}, files)
	if err != nil || got.ID != 2 {
		t.Fatalf("got %+v, %v; want file 2", got, err)
	}
	files[1].Path = "Show/Show S02E04.mkv"
	if got, err := m.pickFileForEpisode(AddRequest{Season: 2, Episode: 3}, files); err == nil {
		t.Fatalf("picked %+v; want no match: the only S02E03-like file is a special", got)
	}
}
