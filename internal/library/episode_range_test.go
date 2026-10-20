package library

import "testing"

func TestParseEpisodeRange(t *testing.T) {
	for name, want := range map[string][3]int{
		"Stargate SG-1 (1997) - S01E01-E02 - Children of the Gods (1080p BluRay x265 Celdra).mkv": {1, 1, 2},
		"01x01-02 [2009 FINAL CUT] Children of the Gods {DVD}.mkv":                                {1, 1, 2},
		"Show.S04E03E04.1080p.mkv":          {4, 3, 4},
		"Show.S02E05-06.mkv":                {2, 5, 6},
		"Show.S01E03.The.Enemy.Within.mkv":  {1, 3, 3},
		"Show.S01E01-1080p.mkv":             {1, 1, 1},
		"Show.S01E01-720p.mkv":              {1, 1, 1},
		"Show.S01E09-E20.mkv":               {1, 9, 9},
		"Show - S01E01 - 2 Broke Girls.mkv": {1, 1, 1},
		"No numbers here.mkv":               {0, 0, 0},
	} {
		s, f, l := ParseEpisodeRange(name)
		if got := [3]int{s, f, l}; got != want {
			t.Errorf("ParseEpisodeRange(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestEpisodeRangeFilename(t *testing.T) {
	if got := EpisodeRangeFilename("Stargate SG-1", 1, 1, 2, "ac9c4dbc"); got != "Stargate_SG-1_S01E01-E02_ac9c4dbc.mkv" {
		t.Fatalf("range name = %q", got)
	}
	if got := EpisodeFilename("Stargate SG-1", 1, 3, "ac9c4dbc"); got != "Stargate_SG-1_S01E03_ac9c4dbc.mkv" {
		t.Fatalf("single name = %q", got)
	}
}

// The Celdra pack holds the two-part pilot as one file. Filed as S01E01 alone,
// E02 stayed a hole, and the gap fill filled it with the same pilot again.
func TestPackEpisodesKeepsARangeTogether(t *testing.T) {
	got := packEpisodes([]FileStat{
		{ID: 10, Path: "SG1/S01/Stargate SG-1 (1997) - S01E01-E02 - Children of the Gods.mkv"},
		{ID: 11, Path: "SG1/S01/Stargate SG-1 (1997) - S01E03 - The Enemy Within.mkv"},
	})
	if len(got) != 2 || got[0].episode != 1 || got[0].last != 2 || got[1].episode != 3 || got[1].last != 3 {
		t.Fatalf("got %+v; want E01-E02 and E03", got)
	}
}
