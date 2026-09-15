package collector

import "testing"

// A season pack is one torrent behind every episode, so the stream shows the
// episodes actually open, once each, and nothing for a movie.
func TestEpisodesOf(t *testing.T) {
	cases := []struct {
		paths []string
		want  string
	}{
		{[]string{"/mnt/real/tv/Show (2020)/Season.01/Show_S01E06_1f68b57d.mkv",
			"/mnt/real/tv/Show (2020)/Season.01/Show_S01E05_1f68b57d.mkv",
			"/mnt/real/tv/Show (2020)/Season.01/Show_S01E05_1f68b57d.mkv"}, "S01E05, S01E06"},
		{[]string{"/mnt/real/movies/Film_2020_1080p_5d414a2f.mkv"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := episodesOf(c.paths); got != c.want {
			t.Errorf("episodesOf(%q) = %q, want %q", c.paths, got, c.want)
		}
	}
}
