package library

import "testing"

func TestPickFileForEpisodeMatchesByName(t *testing.T) {
	m := &Manager{}
	files := []FileStat{
		{ID: 1, Path: "Show.S01E01.mkv", Length: 900},
		{ID: 2, Path: "Show.S01E02.mkv", Length: 800},
	}
	got, err := m.pickFileForEpisode(AddRequest{Season: 1, Episode: 2}, files)
	if err != nil || got.ID != 2 {
		t.Fatalf("got %+v, %v; want file 2", got, err)
	}
}

// A batch numbered absolutely has no SxxExx in its names. Guessing the
// largest file filed one episode's file under hundreds of episode numbers.
func TestPickFileForEpisodeRefusesToGuessInABatch(t *testing.T) {
	m := &Manager{}
	files := []FileStat{
		{ID: 1, Path: "One Piece - 0095.mkv", Length: 900},
		{ID: 2, Path: "One Piece - 0096.mkv", Length: 1000},
	}
	if got, err := m.pickFileForEpisode(AddRequest{Season: 7, Episode: 5}, files); err == nil {
		t.Fatalf("picked %+v; want an error asking for file_index", got)
	}
}

// With one video file the torrent is the episode, whatever the name says.
func TestPickFileForEpisodeSingleVideoFallsBack(t *testing.T) {
	m := &Manager{}
	files := []FileStat{
		{ID: 1, Path: "release.nfo", Length: 10},
		{ID: 2, Path: "Some Episode Title.mkv", Length: 700},
	}
	got, err := m.pickFileForEpisode(AddRequest{Season: 2, Episode: 3}, files)
	if err != nil || got.ID != 2 {
		t.Fatalf("got %+v, %v; want the only video file", got, err)
	}
}

// An explicit file_index skips name matching entirely, which is how a caller
// that knows the file (Torrentio's fileIdx) files an absolutely numbered batch.
func TestPickFileForEpisodeHonoursFileIndex(t *testing.T) {
	m := &Manager{}
	files := []FileStat{
		{ID: 1, Path: "One Piece - 0095.mkv", Length: 900},
		{ID: 2, Path: "One Piece - 0096.mkv", Length: 1000},
	}
	got, err := m.pickFileForEpisode(AddRequest{Season: 7, Episode: 5, FileIndex: 1}, files)
	if err != nil || got.ID != 1 {
		t.Fatalf("got %+v, %v; want file 1", got, err)
	}
}
