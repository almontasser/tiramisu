package engines

import (
	"math"
	"testing"

	"tiramisu/internal/catalog/tmdb"
)

func TestAbsolutelyNumberedSeasonIsLeftAlone(t *testing.T) {
	e := &TVGoEngine{registry: map[string]TVEpisodeEntry{
		// ONE PIECE's season 23 is filed by TMDB's absolute numbers.
		"onepiece1999_s23e1156": {FilePath: "a"},
		// An ordinary season, 1 of 10 filed.
		"onepiece1999_s22e01": {FilePath: "b"},
	}}
	details := &tmdb.TVDetail{Seasons: []tmdb.Season{
		{SeasonNumber: 22, EpisodeCount: 10},
		{SeasonNumber: 23, EpisodeCount: 23},
	}}
	complete := e.getCompleteSeasons("One Piece", "1999-10-20", details)
	if !math.IsInf(complete[23], 1) {
		t.Errorf("season 23 = %v, want it skipped", complete[23])
	}
	if _, ok := complete[22]; ok {
		t.Error("season 22 has 1 of 10 episodes and counts as complete")
	}
}
