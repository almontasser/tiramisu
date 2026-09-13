package engines

import "testing"

// Episodes filed through the Library API were checked by name and translated to
// TMDB's numbering; the sync's TMDB-numbered upgrade must never replace them.
func TestAPIEpisodesAreNeverUpgraded(t *testing.T) {
	best := 1 << 20 // far above any real release score
	if got := protectedScore(0, "api"); float64(best) > float64(got)*tvUpgradeThreshold {
		t.Errorf("an api episode scored %d would be replaced by a release scored %d", got, best)
	}
	if got := protectedScore(1500, "fullpack"); got != 1500 {
		t.Errorf("a sync-filed episode's score changed to %d", got)
	}
	if got := protectedScore(1, "existing"); got != 1 {
		t.Errorf("an adopted stub's score changed to %d", got)
	}
}

func TestFanEditsAreRejected(t *testing.T) {
	for title, want := range map[string]bool{
		"One Pace Official Complete Batch as of July 25 2021":         true,
		"[One Pace][589-590] Post-War 05 [1080p]":                     true,
		"Chronologically Lost (2004) [1080p H265 ENG SUB ENG]":        true,
		"[Anime Time] One Piece - Season 1 - East Blue (Fixed) 1080p": false,
		"Lost.S01.1080p.BluRay.x264-CtrlHD":                           false,
		"Pacey's Recut Scenes":                                        true,
	} {
		if got := reTVFanEdit.MatchString(title); got != want {
			t.Errorf("%q: fan edit = %v, want %v", title, got, want)
		}
	}
}
