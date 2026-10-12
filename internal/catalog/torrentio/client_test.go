package torrentio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// A 429 used to surface as a JSON parse error and be retried on the fallback URL.
func TestRateLimitedBacksOffWithoutFallback(t *testing.T) {
	defer func(b time.Duration) { backoffBase = b }(backoffBase)
	backoffBase = 200 * time.Millisecond

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"streams":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "cfg")
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	ctx := context.Background()

	start := time.Now()
	if _, err := c.FetchEpisodeStreams(ctx, "tt1", 1, 1); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("a 429 was retried on the fallback: %d requests", n)
	}

	// Another client for the same host shares the backoff and gives up with its context.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	other := NewClient(srv.URL, "cfg")
	if _, err := other.FetchMovieStreams(short, "tt2"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context error while backing off, got %v", err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("a request went out during the backoff: %d requests", n)
	}

	if _, err := c.FetchEpisodeStreams(ctx, "tt1", 1, 2); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < backoffBase {
		t.Errorf("second call did not wait out the backoff: %s", waited)
	}
	if c.backoff.delay != 0 {
		t.Errorf("a success should reset the backoff, delay=%s", c.backoff.delay)
	}
}

func TestBackoffDelays(t *testing.T) {
	b := &backoff{}
	if d := b.hit("120"); d != 120*time.Second {
		t.Errorf("Retry-After 120: got %s", d)
	}
	if d := b.hit("86400"); d != backoffMax {
		t.Errorf("Retry-After is capped: got %s", d)
	}
	want := []time.Duration{backoffBase, 2 * backoffBase, 4 * backoffBase}
	for _, w := range want {
		if d := b.hit(""); d != w {
			t.Errorf("exponential: got %s, want %s", d, w)
		}
	}
	for range 10 {
		b.hit("")
	}
	if d := b.hit(""); d != backoffMax {
		t.Errorf("exponential is capped: got %s", d)
	}
}

func TestNon2xxIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "cfg")
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	_, err := c.FetchMovieStreams(context.Background(), "tt1")
	if err == nil || err.Error() != "torrentio HTTP 404" {
		t.Errorf("want torrentio HTTP 404, got %v", err)
	}
}
