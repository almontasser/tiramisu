package library

import (
	"context"
	"testing"
)

// fakeGoStorm answers with a fixed file list. Inspect only reads the engine, so
// this is all a classification test needs.
type fakeGoStorm struct {
	stats TorrentStats
	err   error
}

func (f *fakeGoStorm) AddTorrent(ctx context.Context, magnet, title string) (string, error) {
	return f.stats.Hash, nil
}

func (f *fakeGoStorm) GetTorrentInfo(ctx context.Context, hash string, maxWait int) (*TorrentStats, error) {
	if f.err != nil {
		return nil, f.err
	}
	s := f.stats
	return &s, nil
}

func (f *fakeGoStorm) RemoveTorrent(ctx context.Context, hash string) error { return nil }

func (f *fakeGoStorm) ListTorrents(ctx context.Context) ([]TorrentStats, error) { return nil, nil }

func files(paths ...string) []FileStat {
	out := make([]FileStat, 0, len(paths))
	for i, p := range paths {
		// Descending sizes, so the first path given is the largest file and a
		// test can express "the feature is this one" by ordering.
		out = append(out, FileStat{ID: i + 1, Path: p, Length: int64(1000 - i)})
	}
	return out
}

const testHash = "f12d8fd29e4fce1d54968698b23e00b104d6b9fc"

func inspectWith(t *testing.T, title string, fs []FileStat) *InspectResponse {
	t.Helper()
	m := &Manager{cfg: Config{GoStorm: &fakeGoStorm{
		stats: TorrentStats{Hash: testHash, Title: title, Length: 1234, FileStats: fs},
	}}}
	got, err := m.Inspect(context.Background(), InspectRequest{Hash: testHash})
	if err != nil {
		t.Fatalf("Inspect(%q): %v", title, err)
	}
	return got
}

func TestInspectClassifies(t *testing.T) {
	tests := []struct {
		name      string
		title     string
		files     []FileStat
		wantKind  string
		wantPack  bool
		wantSeas  int
		wantEp    int
		wantIndex int
		wantRes   string
	}{
		{
			name:      "single feature",
			title:     "Inception.2010.1080p.BluRay.x264",
			files:     files("Inception.2010.1080p.BluRay.x264.mkv"),
			wantKind:  "movie",
			wantIndex: 1,
			wantRes:   "1080p",
		},
		{
			name:  "feature with a sample is still one feature",
			title: "Some.Movie.2021.2160p.UHD",
			files: files(
				"Some.Movie.2021.2160p.mkv",
				"sample/Some.Movie-sample.mkv",
			),
			wantKind:  "movie",
			wantIndex: 1,
			wantRes:   "2160p",
		},
		{
			name:      "single episode",
			title:     "Show.S03E07.1080p.WEB-DL",
			files:     files("Show.S03E07.1080p.WEB-DL.mkv"),
			wantKind:  "tv",
			wantSeas:  3,
			wantEp:    7,
			wantIndex: 1,
			wantRes:   "1080p",
		},
		{
			name:  "season pack",
			title: "Show.S02.COMPLETE.1080p",
			files: files(
				"Show.S02E01.mkv", "Show.S02E02.mkv", "Show.S02E03.mkv",
			),
			wantKind: "tv",
			wantPack: true,
			wantSeas: 2,
			wantRes:  "1080p",
		},
		{
			name:  "multi-season pack has no single season",
			title: "Show.Complete.Series.720p",
			files: files(
				"S01/Show.S01E01.mkv", "S01/Show.S01E02.mkv",
				"S02/Show.S02E01.mkv",
			),
			wantKind: "tv",
			wantPack: true,
			wantSeas: 0,
			wantRes:  "720p",
		},
		{
			name: "unnumbered files plus a season in the name is still a pack",
			// Some packs name episodes by title only; the season marker on the
			// torrent is then the only evidence it is not a film.
			title: "Show.Season 4.1080p",
			files: files(
				"01 - Pilot.mkv", "02 - The Return.mkv",
			),
			wantKind: "tv",
			wantPack: true,
			wantSeas: 4,
			wantRes:  "1080p",
		},
		{
			name:  "non-video files are ignored when picking the feature",
			title: "Film.2019.1080p",
			files: []FileStat{
				{ID: 1, Path: "Film.2019.1080p.nfo", Length: 9999},
				{ID: 2, Path: "Film.2019.1080p.mkv", Length: 500},
			},
			wantKind:  "movie",
			wantIndex: 2,
			wantRes:   "1080p",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := inspectWith(t, tc.title, tc.files)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q (note: %s)", got.Kind, tc.wantKind, got.Note)
			}
			if got.Pack != tc.wantPack {
				t.Errorf("Pack = %v, want %v (note: %s)", got.Pack, tc.wantPack, got.Note)
			}
			if got.Season != tc.wantSeas {
				t.Errorf("Season = %d, want %d (note: %s)", got.Season, tc.wantSeas, got.Note)
			}
			if got.Episode != tc.wantEp {
				t.Errorf("Episode = %d, want %d", got.Episode, tc.wantEp)
			}
			if tc.wantIndex != 0 && got.FileIndex != tc.wantIndex {
				t.Errorf("FileIndex = %d, want %d", got.FileIndex, tc.wantIndex)
			}
			if got.Resolution != tc.wantRes {
				t.Errorf("Resolution = %q, want %q", got.Resolution, tc.wantRes)
			}
			if got.Note == "" {
				t.Error("Note is empty; the form relies on it to explain the guess")
			}
		})
	}
}

func TestInspectRejectsBadHash(t *testing.T) {
	m := &Manager{cfg: Config{GoStorm: &fakeGoStorm{}}}
	if _, err := m.Inspect(context.Background(), InspectRequest{Hash: "nope"}); err == nil {
		t.Fatal("expected an error for a malformed info hash")
	}
}

// A pack must not report an episode: AddRequest treats episode 0 with a season
// set as "file the whole pack", and any other value would file one stub.
func TestInspectPackHasNoEpisode(t *testing.T) {
	got := inspectWith(t, "Show.S05.1080p", files("Show.S05E01.mkv", "Show.S05E02.mkv"))
	if got.Episode != 0 {
		t.Fatalf("pack reported Episode = %d, want 0", got.Episode)
	}
}
