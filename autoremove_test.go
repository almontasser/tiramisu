package main

import "testing"

// A removed movie must blacklist its own title, not the folder all movies share.
func TestDeriveTitleFromPath(t *testing.T) {
	tr := &TorrentRemover{}
	for path, want := range map[string]string{
		"/mnt/real/movies/I_Want_Your_Sex_2026_1080p_fc638b71.mkv":       "I_Want_Your_Sex",
		"/mnt/real/movies/28_Days_Later_2002_1080p_5.1_3042c692.mkv":     "28_Days_Later",
		"/mnt/real/movies/Blade_Runner_2049_2017_2160p_HDR_0a1b2c3d.mkv": "Blade_Runner_2049",
		"/mnt/real/movies/Untitled_1080p_0a1b2c3d.mkv":                   "Untitled",
		"/mnt/real/tv/9-1-1_Nashville (2025)/Season.01/x_S01E01_aa.mkv":  "9-1-1_Nashville (2025)",
		"/mnt/real/tv/Show_2020_1080p (2020)/Season.01/x_S01E01_aa.mkv":  "Show_2020_1080p (2020)",
		"/mnt/real/movies/not-a-stub.mkv":                                "movies",
	} {
		if got := tr.deriveTitleFromPath(path); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}
