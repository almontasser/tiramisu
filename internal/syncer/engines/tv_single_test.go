package engines

import (
	"testing"

	"tiramisu/internal/library"
)

const gb = int64(1) << 30

// Torrentio answered American Horror Story S01E02 with the seasons 1-8 pack, and
// its largest file, S05E02, was filed as S01E01 and S01E02.
func TestSingleEpisodeFileTakesTheNamedFile(t *testing.T) {
	files := []FileStat{
		{ID: 1, Path: "AHS 1-8/S01/American Horror Story - S01E01 - Pilot.mp4", Length: 2 * gb},
		{ID: 2, Path: "AHS 1-8/S01/American Horror Story - S01E02 - Home Invasion.mp4", Length: 2 * gb},
		{ID: 72, Path: "AHS 1-8/S05/American Horror Story - S05E02 - Chutes and Ladders.mp4", Length: 4 * gb},
	}
	got, last := singleEpisodeFile(files, 1, 2, library.IsVideoFile)
	if got == nil || got.ID != 2 || last != 2 {
		t.Fatalf("got %+v, last %d; want file 2", got, last)
	}
	if got, _ := singleEpisodeFile(files, 1, 3, library.IsVideoFile); got != nil {
		t.Fatalf("got %+v for an episode the pack does not name; want nothing", got)
	}
}

func TestSingleEpisodeFileRangeAndLoneFile(t *testing.T) {
	got, last := singleEpisodeFile([]FileStat{
		{ID: 1, Path: "Season 01/01x01-02 Children of the Gods.mkv", Length: 5 * gb},
		{ID: 2, Path: "Season 01/01x03 The Enemy Within.mkv", Length: 2 * gb},
	}, 1, 1, library.IsVideoFile)
	if got == nil || got.ID != 1 || last != 2 {
		t.Fatalf("got %+v, last %d; want file 1 holding E01-E02", got, last)
	}
	// Part 2 alone is not in that file: E02 must not be filed from it.
	if got, _ := singleEpisodeFile([]FileStat{{ID: 1, Path: "01x01-02 Children of the Gods.mkv", Length: 5 * gb}}, 1, 2, library.IsVideoFile); got != nil {
		t.Fatalf("got %+v; want nothing for E02", got)
	}
	got, _ = singleEpisodeFile([]FileStat{{ID: 3, Path: "Episode Title.mkv", Length: 2 * gb}, {ID: 4, Path: "sample.txt", Length: 2 * gb}}, 1, 5, library.IsVideoFile)
	if got == nil || got.ID != 3 {
		t.Fatalf("got %+v; want the lone video file", got)
	}
}
