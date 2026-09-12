package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TMDBCatalog is a ShowCatalog backed by TMDB.
//
// A report covers every series, so a cold load makes a search and a details
// request per show: a few hundred requests. The caches make that a one-time
// cost. A title match is kept for the life of the process, because a show does
// not change which TMDB entry it is. Aired counts are kept for six hours, which
// is often enough to notice a new episode of a running show.
type TMDBCatalog struct {
	key    func() string
	base   string
	client *http.Client
	ttl    time.Duration

	mu    sync.Mutex
	shows map[string]showMatch
	aired map[int]airedCounts
}

type showMatch struct {
	show CatalogShow
	err  string
}

type airedCounts struct {
	at      time.Time
	seasons map[int]int
}

// NewTMDBCatalog returns a catalogue that reads its API key through key on
// every request, so a key changed in config.json applies without a restart.
func NewTMDBCatalog(key func() string) *TMDBCatalog {
	return &TMDBCatalog{
		key:    key,
		base:   "https://api.themoviedb.org/3",
		client: &http.Client{Timeout: 15 * time.Second},
		ttl:    6 * time.Hour,
		shows:  map[string]showMatch{},
		aired:  map[int]airedCounts{},
	}
}

// Configured reports whether an API key is set.
func (c *TMDBCatalog) Configured() bool { return strings.TrimSpace(c.key()) != "" }

// Reset drops every cached answer.
func (c *TMDBCatalog) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shows = map[string]showMatch{}
	c.aired = map[int]airedCounts{}
}

func (c *TMDBCatalog) get(ctx context.Context, path string, q url.Values, out interface{}) error {
	q.Set("api_key", c.key())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("TMDB request for %s: %v", path, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		// A *url.Error prints the whole URL, and the URL carries the API key.
		// The report is served to the browser, so report only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("TMDB request for %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("TMDB answered %s for %s", resp.Status, path)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func normTitle(s string) string { return reNonWord.ReplaceAllString(strings.ToLower(s), "") }

// FindShow resolves a folder's title and year. An exact title match wins; when
// no result matches exactly, the first result is used and its name is returned
// so the report shows which entry it compared against.
func (c *TMDBCatalog) FindShow(ctx context.Context, title, year string) (CatalogShow, error) {
	cacheKey := strings.ToLower(title) + "|" + year
	c.mu.Lock()
	if m, ok := c.shows[cacheKey]; ok {
		c.mu.Unlock()
		if m.err != "" {
			return CatalogShow{}, errors.New(m.err)
		}
		return m.show, nil
	}
	c.mu.Unlock()

	type result struct {
		ID           int    `json:"id"`
		Name         string `json:"name"`
		OriginalName string `json:"original_name"`
	}
	search := func(withYear bool) ([]result, error) {
		q := url.Values{"query": {title}}
		if withYear && year != "" {
			q.Set("first_air_date_year", year)
		}
		var res struct {
			Results []result `json:"results"`
		}
		err := c.get(ctx, "/search/tv", q, &res)
		return res.Results, err
	}
	results, err := search(true)
	if err == nil && len(results) == 0 && year != "" {
		// A folder's year can be a year off TMDB's first air date, for a pilot
		// or a regional premiere. Retry without it rather than report no match.
		results, err = search(false)
	}
	if err != nil {
		return CatalogShow{}, err // transient: not cached
	}

	var m showMatch
	want := normTitle(title)
	for _, r := range results {
		if normTitle(r.Name) == want || normTitle(r.OriginalName) == want {
			m.show = CatalogShow{ID: r.ID, Name: r.Name}
			break
		}
	}
	if m.show.ID == 0 {
		if len(results) > 0 {
			m.show = CatalogShow{ID: results[0].ID, Name: results[0].Name}
		} else {
			m.err = fmt.Sprintf("no TMDB match for %q", title)
		}
	}
	c.mu.Lock()
	c.shows[cacheKey] = m
	c.mu.Unlock()
	if m.err != "" {
		return CatalogShow{}, errors.New(m.err)
	}
	return m.show, nil
}

// AiredEpisodes returns aired episodes per season for a TMDB show id.
func (c *TMDBCatalog) AiredEpisodes(ctx context.Context, id int) (map[int]int, error) {
	c.mu.Lock()
	if a, ok := c.aired[id]; ok && time.Since(a.at) < c.ttl {
		c.mu.Unlock()
		return a.seasons, nil
	}
	c.mu.Unlock()

	var d struct {
		Seasons []struct {
			SeasonNumber int `json:"season_number"`
			EpisodeCount int `json:"episode_count"`
		} `json:"seasons"`
		LastEpisodeToAir *struct {
			SeasonNumber  int `json:"season_number"`
			EpisodeNumber int `json:"episode_number"`
		} `json:"last_episode_to_air"`
	}
	if err := c.get(ctx, "/tv/"+strconv.Itoa(id), url.Values{}, &d); err != nil {
		return nil, err
	}
	seasons := map[int]int{}
	for _, s := range d.Seasons {
		seasons[s.SeasonNumber] = s.EpisodeCount
	}
	lastS, lastE := 0, 0
	if d.LastEpisodeToAir != nil {
		lastS, lastE = d.LastEpisodeToAir.SeasonNumber, d.LastEpisodeToAir.EpisodeNumber
	}
	aired := AiredFromSeasons(seasons, lastS, lastE)

	c.mu.Lock()
	c.aired[id] = airedCounts{at: time.Now(), seasons: aired}
	c.mu.Unlock()
	return aired, nil
}
