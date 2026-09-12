package library

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// InspectRequest asks what is inside a torrent without filing anything.
type InspectRequest struct {
	Magnet string `json:"magnet"`
	Hash   string `json:"hash"`
	// Title only helps the engine label the torrent while metadata is fetched.
	Title        string `json:"title"`
	MetadataWait int    `json:"metadata_wait"`
}

// InspectFile is one file inside the torrent, with whatever the name gives away.
type InspectFile struct {
	ID      int    `json:"id"`
	Path    string `json:"path"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Video   bool   `json:"video"`
	Season  int    `json:"season,omitempty"`
	Episode int    `json:"episode,omitempty"`
}

// InspectResponse is the torrent's contents plus a guess at how it should be filed.
// Every guessed field is a suggestion for the form to prefill, never a decision:
// the caller still sends an explicit AddRequest.
type InspectResponse struct {
	Hash       string        `json:"hash"`
	Title      string        `json:"title"`
	Size       int64         `json:"size"`
	Files      []InspectFile `json:"files"`
	VideoCount int           `json:"video_count"`

	// Kind is "movie" or "tv": a torrent whose files carry episode numbers is
	// a series regardless of what the form currently has selected.
	Kind string `json:"kind"`
	// Pack means more than one episode is inside, so it should be filed as a
	// pack (episode 0) rather than as one episode.
	Pack       bool   `json:"pack"`
	Seasons    []int  `json:"seasons"`
	Season     int    `json:"season,omitempty"`
	Episode    int    `json:"episode,omitempty"`
	FileIndex  int    `json:"file_index"`
	Resolution string `json:"resolution,omitempty"`
	// Note explains the guess in one line, so the form can show its reasoning
	// instead of silently changing fields under the user.
	Note string `json:"note"`
}

var (
	// "S02" / "Season 2" with no episode: a season pack whose files may not
	// each carry SxxExx.
	reSeasonOnly = regexp.MustCompile(`(?i)\b(?:s|season[ ._-]*)(\d{1,2})\b`)
	reRes2160    = regexp.MustCompile(`(?i)\b(2160p|4k|uhd)\b`)
	reRes1080    = regexp.MustCompile(`(?i)\b1080p\b`)
	reRes720     = regexp.MustCompile(`(?i)\b720p\b`)
	// Extras that are video files but never the feature.
	reJunk = regexp.MustCompile(`(?i)\b(sample|trailer|extra|featurette|behind[ ._-]the[ ._-]scenes|deleted[ ._-]scene)\b`)
)

func resolutionOf(s string) string {
	switch {
	case reRes2160.MatchString(s):
		return "2160p"
	case reRes1080.MatchString(s):
		return "1080p"
	case reRes720.MatchString(s):
		return "720p"
	}
	return ""
}

// Inspect registers the torrent with the engine long enough to read its file
// list, then classifies it.
//
// Filing decisions hinge on things only the torrent's contents reveal - whether
// it holds one episode or a whole season, which file is the feature rather than
// a sample - and until now the only way to find out was to file it and look at
// what appeared.
func (m *Manager) Inspect(ctx context.Context, req InspectRequest) (*InspectResponse, error) {
	hash := strings.ToLower(strings.TrimSpace(req.Hash))
	magnet := strings.TrimSpace(req.Magnet)
	if hash == "" && magnet != "" {
		hash = HashFromMagnet(magnet)
	}
	if !reInfoHash.MatchString(hash) {
		return nil, errf(http.StatusBadRequest, "need a magnet or a 40-character info hash")
	}
	if magnet == "" {
		magnet = BuildMagnet(hash, req.Title, DefaultTrackers())
	}

	if _, err := m.cfg.GoStorm.AddTorrent(ctx, magnet, req.Title); err != nil {
		return nil, errf(http.StatusBadGateway, "engine refused the torrent: %v", err)
	}
	wait := req.MetadataWait
	if wait <= 0 {
		wait = 60
	}
	stats, err := m.cfg.GoStorm.GetTorrentInfo(ctx, hash, wait)
	if err != nil {
		return nil, errf(http.StatusGatewayTimeout,
			"no metadata after %ds - the swarm may be dead: %v", wait, err)
	}

	out := &InspectResponse{Hash: hash, Title: stats.Title, Size: stats.Length, Files: []InspectFile{}}

	var biggest InspectFile
	seasonSet := map[int]bool{}
	episodes := 0
	for _, f := range stats.FileStats {
		name := filepath.Base(f.Path)
		season, episode := ParseSeasonEpisode(name)
		item := InspectFile{
			ID: f.ID, Path: f.Path, Name: name, Size: f.Length,
			Video: IsVideoFile(f.Path), Season: season, Episode: episode,
		}
		out.Files = append(out.Files, item)
		if !item.Video || reJunk.MatchString(name) {
			continue
		}
		out.VideoCount++
		if item.Size > biggest.Size {
			biggest = item
		}
		if episode > 0 && season > 0 && !IsExtrasPath(f.Path) {
			episodes++
			seasonSet[season] = true
		}
	}
	if len(out.Files) == 0 {
		return nil, errf(http.StatusBadGateway, "engine returned no files for %s", hash)
	}

	// The engine reports a torrent-level length of 0 for some torrents even
	// though every file length is right, which showed as "0 B" next to a 9 GB
	// season pack. The files are the authority.
	if out.Size <= 0 {
		for _, f := range out.Files {
			out.Size += f.Size
		}
	}

	for s := range seasonSet {
		out.Seasons = append(out.Seasons, s)
	}
	sort.Ints(out.Seasons)

	// The stream URL addresses files by their 1-based id; 0 means "largest video
	// file", which is what a single-feature torrent wants anyway.
	out.FileIndex = 0
	out.Resolution = resolutionOf(stats.Title)
	if out.Resolution == "" {
		out.Resolution = resolutionOf(biggest.Name)
	}

	switch {
	case episodes > 1:
		out.Kind, out.Pack = "tv", true
		if len(out.Seasons) == 1 {
			out.Season = out.Seasons[0]
			out.Note = plural(episodes, "episode") + " of season " +
				itoa(out.Season) + " — file as a season pack"
		} else {
			out.Note = plural(episodes, "episode") + " across seasons " +
				joinInts(out.Seasons) + " — file as a multi-season pack"
		}

	case episodes == 1:
		out.Kind = "tv"
		out.Season, out.Episode = biggest.Season, biggest.Episode
		out.FileIndex = biggest.ID
		out.Note = "single episode S" + pad2(out.Season) + "E" + pad2(out.Episode)

	default:
		// No episode numbering anywhere. A season marker in the torrent name
		// still means a pack whose files simply are not numbered.
		if mm := reSeasonOnly.FindStringSubmatch(stats.Title); mm != nil && out.VideoCount > 1 {
			out.Kind, out.Pack = "tv", true
			out.Season = atoiSafe(mm[1])
			out.Seasons = []int{out.Season}
			out.Note = plural(out.VideoCount, "video file") +
				" and season " + itoa(out.Season) + " in the name — file as a season pack"
			break
		}
		out.Kind = "movie"
		out.FileIndex = biggest.ID
		if out.VideoCount > 1 {
			out.Note = plural(out.VideoCount, "video file") +
				" with no episode numbering — largest picked (" + biggest.Name + ")"
		} else {
			out.Note = "single feature"
		}
	}
	return out, nil
}

// Small formatting helpers for the Note line. Kept local: they exist only to
// make the explanation readable, not as general utilities.

func itoa(n int) string { return strconv.Itoa(n) }

func pad2(n int) string { return fmt.Sprintf("%02d", n) }

func atoiSafe(s string) int { n, _ := strconv.Atoi(s); return n }

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func joinInts(v []int) string {
	parts := make([]string, 0, len(v))
	for _, n := range v {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}
