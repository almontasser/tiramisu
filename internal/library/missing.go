package library

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ShowCatalog reports how many episodes of a show have aired. The missing
// report compares that with the stubs on disk. TMDB is the implementation,
// because Jellyfin identifies episodes by TMDB's numbering.
type ShowCatalog interface {
	// FindShow resolves the title and year that a show folder carries.
	FindShow(ctx context.Context, title, year string) (CatalogShow, error)
	// AiredEpisodes returns the aired episode count per season, without specials.
	AiredEpisodes(ctx context.Context, id int) (map[int]int, error)
}

// CatalogShow is one catalogue entry.
type CatalogShow struct {
	ID   int
	Name string
}

// MismatchThreshold is how many filed episodes beyond the catalogue's count
// mark a show as numbered differently. One or two can be a misfiled special;
// three is a pattern, such as BluRay packs that merge two-part episodes. A
// mismatched show's gaps point at the wrong episodes, so they are reported but
// kept out of the per-category totals.
const MismatchThreshold = 3

// MissingSeason is one season with episodes the library does not have.
type MissingSeason struct {
	Season   int   `json:"season"`
	Expected int   `json:"expected"`
	Missing  []int `json:"missing"`
	// Category is "whole" when no episode is filed, "only_e01" when episode 1
	// is the only one, and "partial" otherwise.
	Category string `json:"category"`
}

// MissingShow compares one show folder with its catalogue entry.
type MissingShow struct {
	Tree     string          `json:"tree"`
	Folder   string          `json:"folder"`
	Title    string          `json:"title"`
	Year     string          `json:"year,omitempty"`
	TMDBID   int             `json:"tmdb_id,omitempty"`
	TMDBName string          `json:"tmdb_name,omitempty"`
	Aired    int             `json:"aired"`
	Filed    int             `json:"filed"`
	Missing  int             `json:"missing"`
	Outside  int             `json:"outside"`
	Mismatch bool            `json:"mismatch"`
	Seasons  []MissingSeason `json:"seasons,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// MissingTotals sums a report.
type MissingTotals struct {
	Shows      int            `json:"shows"`
	Aired      int            `json:"aired"`
	Filed      int            `json:"filed"`
	Missing    int            `json:"missing"`
	Mismatch   int            `json:"mismatch"`
	ByCategory map[string]int `json:"by_category"`
}

// MissingReport is the answer to GET /api/library/missing.
type MissingReport struct {
	Generated time.Time     `json:"generated"`
	Shows     []MissingShow `json:"shows"`
	Totals    MissingTotals `json:"totals"`
}

var (
	reEpisodeStub = regexp.MustCompile(`_S(\d+)E(\d+)_[0-9a-f]{8}\.mkv$`)
	reFolderYear  = regexp.MustCompile(`^(.*) \((\d{4})\)$`)
)

// TitleFromFolder recovers the title and year a show folder was named from.
//
// ShowFolderName builds the folder from SanitizeShowName(title) and the year,
// and the sanitiser only removes characters and turns spaces into underscores.
// Turning underscores back into spaces therefore gives a title that names the
// same folder and the same registry keys.
func TitleFromFolder(folder string) (title, year string) {
	base := folder
	if m := reFolderYear.FindStringSubmatch(folder); m != nil {
		base, year = m[1], m[2]
	}
	return strings.TrimSpace(strings.ReplaceAll(base, "_", " ")), year
}

// AiredFromSeasons turns a catalogue's season list into aired counts. Season 0
// holds specials, which the library never files by number, and a season past
// the last aired episode has not started. lastSeason 0 means the catalogue
// gave no last-aired episode, and every listed season counts.
func AiredFromSeasons(seasons map[int]int, lastSeason, lastEpisode int) map[int]int {
	out := map[int]int{}
	for s, count := range seasons {
		if s <= 0 || count <= 0 || (lastSeason > 0 && s > lastSeason) {
			continue
		}
		if s == lastSeason && lastEpisode > 0 && lastEpisode < count {
			count = lastEpisode
		}
		out[s] = count
	}
	return out
}

type catalogConfigured interface{ Configured() bool }
type catalogResetter interface{ Reset() }

// ResetMissingCache drops the catalogue answers the report has cached, so the
// next report asks again.
func (m *Manager) ResetMissingCache() {
	if r, ok := m.cfg.Catalog.(catalogResetter); ok {
		r.Reset()
	}
}

// Missing compares every series folder with the episodes its catalogue entry
// says have aired. kind is "tv", "anime", or empty or "all" for both trees.
func (m *Manager) Missing(ctx context.Context, kind string) (*MissingReport, error) {
	if m.cfg.Catalog == nil {
		return nil, errf(http.StatusServiceUnavailable, "the missing-episode report needs a TMDB catalogue")
	}
	if c, ok := m.cfg.Catalog.(catalogConfigured); ok && !c.Configured() {
		return nil, errf(http.StatusServiceUnavailable, "the missing-episode report needs a TMDB API key: set tmdb_api_key in config.json")
	}

	type job struct{ tree, dir, folder string }
	var jobs []job
	want := strings.ToLower(strings.TrimSpace(kind))
	for _, t := range []struct{ name, dir string }{{"tv", m.cfg.TVDir}, {"anime", m.cfg.AnimeDir}} {
		if t.dir == "" || (want != "" && want != "all" && want != t.name) {
			continue
		}
		entries, err := os.ReadDir(t.dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, errf(http.StatusInternalServerError, "cannot read %s: %v", t.dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				jobs = append(jobs, job{t.name, t.dir, e.Name()})
			}
		}
	}

	// A cold report makes two catalogue requests per show. Eight at a time
	// keeps a first load to seconds without tripping TMDB's rate limit.
	shows := make([]MissingShow, len(jobs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			shows[i] = m.missingFor(ctx, j.tree, filepath.Join(j.dir, j.folder), j.folder)
		}(i, j)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, errf(http.StatusRequestTimeout, "report cancelled: %v", err)
	}

	sort.SliceStable(shows, func(a, b int) bool {
		if shows[a].Missing != shows[b].Missing {
			return shows[a].Missing > shows[b].Missing
		}
		return shows[a].Folder < shows[b].Folder
	})
	rep := &MissingReport{Generated: time.Now().UTC(), Shows: shows}
	rep.Totals.ByCategory = map[string]int{}
	for _, s := range shows {
		rep.Totals.Shows++
		rep.Totals.Aired += s.Aired
		rep.Totals.Filed += s.Filed
		rep.Totals.Missing += s.Missing
		if s.Mismatch {
			rep.Totals.Mismatch++
			continue
		}
		for _, season := range s.Seasons {
			rep.Totals.ByCategory[season.Category] += len(season.Missing)
		}
	}
	return rep, nil
}

func (m *Manager) missingFor(ctx context.Context, tree, dir, folder string) MissingShow {
	title, year := TitleFromFolder(folder)
	show := MissingShow{Tree: tree, Folder: folder, Title: title, Year: year}

	filed := map[int]map[int]bool{}
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		mm := reEpisodeStub.FindStringSubmatch(d.Name())
		if mm == nil {
			return nil
		}
		s, _ := strconv.Atoi(mm[1])
		e, _ := strconv.Atoi(mm[2])
		if filed[s] == nil {
			filed[s] = map[int]bool{}
		}
		if !filed[s][e] {
			filed[s][e] = true
			show.Filed++
		}
		return nil
	})

	if err := ctx.Err(); err != nil {
		show.Error = err.Error()
		return show
	}
	entry, err := m.cfg.Catalog.FindShow(ctx, title, year)
	if err != nil {
		show.Error = err.Error()
		return show
	}
	show.TMDBID, show.TMDBName = entry.ID, entry.Name
	aired, err := m.cfg.Catalog.AiredEpisodes(ctx, entry.ID)
	if err != nil {
		show.Error = err.Error()
		return show
	}

	for s, eps := range filed {
		if s <= 0 {
			continue // specials are not numbered against the catalogue
		}
		for e := range eps {
			if e > aired[s] {
				show.Outside++
			}
		}
	}
	show.Mismatch = show.Outside >= MismatchThreshold

	seasons := make([]int, 0, len(aired))
	for s := range aired {
		seasons = append(seasons, s)
	}
	sort.Ints(seasons)
	for _, s := range seasons {
		count := aired[s]
		show.Aired += count
		var missing []int
		have := 0
		for e := 1; e <= count; e++ {
			if filed[s][e] {
				have++
			} else {
				missing = append(missing, e)
			}
		}
		if len(missing) == 0 {
			continue
		}
		category := "partial"
		switch {
		case have == 0:
			category = "whole"
		case have == 1 && filed[s][1]:
			category = "only_e01"
		}
		show.Seasons = append(show.Seasons, MissingSeason{Season: s, Expected: count, Missing: missing, Category: category})
		show.Missing += len(missing)
	}
	return show
}
