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

// A pack of films: the file named for the movie, not the largest, and a year in
// the name tells The Dark Knight from The Dark Knight Rises.
func TestMovieFileNamesTheMovieInAPack(t *testing.T) {
	files := []FileStat{
		{ID: 5, Path: "Amazing Films 11/DK1 Batman Begins (2005) 1080p Surround.mp4", Length: 4 << 30},
		{ID: 12, Path: "Amazing Films 11/DK2 The Dark Knight (2008) 1080p Surround.mp4", Length: 4 << 30},
		{ID: 13, Path: "Amazing Films 11/DK3 The Dark Knight Rises (2012) 1080p Surround.mp4", Length: 5 << 30},
		{ID: 14, Path: "Amazing Films 11/Spiderman 3 (2007) 1080p x264.mkv", Length: 3 << 30},
	}
	if got := MovieFile(files, "The Dark Knight", 2008); got == nil || got.ID != 12 {
		t.Fatalf("got %+v; want file 12", got)
	}
	if got := MovieFile(files, "Spider-Man 3", 2007); got == nil || got.ID != 14 {
		t.Fatalf("got %+v; want file 14, named with the hyphen squeezed out", got)
	}
}

// With no file named for the movie, a pack is refused: the largest file of "30
// Movies That Make Men Cry" was filed as Good Will Hunting.
func TestMovieFileRefusesAnUnnamedPack(t *testing.T) {
	files := []FileStat{
		{ID: 60, Path: "Good.Will.Hunting.1997.1080p.mp4", Length: 2 << 30},
		{ID: 73, Path: "Some.Other.Film.2003.1080p.REMUX.mkv", Length: 38 << 30},
		{ID: 74, Path: "Another.Film.2009.1080p.mkv", Length: 2 << 30},
	}
	if got := MovieFile(files, "The Boy and the Beast", 2015); got != nil {
		t.Fatalf("picked %+v; want nil", got)
	}
}

// A single film ships with a sample, a small unmarked extra and a text file; its
// name need not match the title at all.
func TestMovieFileTakesTheOneFeature(t *testing.T) {
	files := []FileStat{
		{ID: 1, Path: "Bakemono.no.Ko.2015.1080p/Bakemono.no.Ko.2015.1080p.mkv", Length: 6 << 30},
		{ID: 2, Path: "Bakemono.no.Ko.2015.1080p/Sample/sample.mkv", Length: 80 << 20},
		{ID: 3, Path: "Bakemono.no.Ko.2015.1080p/intro.mp4", Length: 200 << 20},
		{ID: 4, Path: "Bakemono.no.Ko.2015.1080p/RARBG.txt", Length: 1 << 10},
	}
	if got := MovieFile(files, "The Boy and the Beast", 2015); got == nil || got.ID != 1 {
		t.Fatalf("got %+v; want file 1", got)
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
