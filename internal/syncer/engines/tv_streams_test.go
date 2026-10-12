package engines

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/time/rate"

	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/catalog/torrentio"
)

// torrentioStub answers every request with status and records the paths asked for.
func torrentioStub(t *testing.T, status int) (*TVGoEngine, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "Too Many Requests", status)
			return
		}
		w.Write([]byte(`{"streams":[]}`))
	}))
	t.Cleanup(srv.Close)
	e := &TVGoEngine{
		torrentio: torrentio.NewClient(srv.URL, "cfg"),
		limiter:   rate.NewLimiter(rate.Inf, 1),
		logger:    log.New(io.Discard, "", 0),
	}
	return e, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
}

var twoSeasons = &tmdb.TVDetail{NumberOfSeasons: 2, Seasons: []tmdb.Season{
	{SeasonNumber: 1, EpisodeCount: 3},
	{SeasonNumber: 2, EpisodeCount: 1},
}}

// A season already complete at a good score used to be fetched episode by episode
// and only dropped when the streams were filtered.
func TestSkippedSeasonIsNotFetched(t *testing.T) {
	e, paths := torrentioStub(t, http.StatusOK)
	_, searched := e.getStreams(context.Background(), "tt1", 1, "Show", twoSeasons, map[int]bool{1: true})

	got := paths()
	if len(got) != 1 || !strings.HasSuffix(got[0], "tt1:2:1.json") {
		t.Errorf("want only S02E01 fetched, got %v", got)
	}
	if _, ok := searched[1]; ok {
		t.Errorf("a skipped season must not read as searched: %+v", searched[1])
	}
	if !searched[2].Complete {
		t.Errorf("season 2 was fetched in full: %+v", searched[2])
	}
}

// One 429 ends the fallback for the show instead of walking every episode.
func TestRateLimitEndsTheFallback(t *testing.T) {
	e, paths := torrentioStub(t, http.StatusTooManyRequests)
	_, searched := e.getStreams(context.Background(), "tt1", 1, "Show", twoSeasons, nil)

	if got := paths(); len(got) != 1 {
		t.Errorf("want one request, got %v", got)
	}
	for _, s := range []int{1, 2} {
		if searched[s].Complete {
			t.Errorf("season %d reads as searched after a 429", s)
		}
	}
}
