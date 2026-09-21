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

// InspectRequest identifies a torrent to read the file list of. It selects
// nothing: naming and file choice stay with the caller.
type InspectRequest struct {
	Hash         string `json:"hash"`
	Magnet       string `json:"magnet"`
	Title        string `json:"title"`
	MetadataWait int    `json:"metadata_wait"`
}

// InspectFile is one source file as the engine sees it. source_path is what a
// caller selects by; file_index is GoStorm's own resolved state. The rest is
// what the name gives away.
type InspectFile struct {
	SourcePath string `json:"source_path"`
	FileIndex  int    `json:"file_index"`
	Size       int64  `json:"size"`
	Name       string `json:"name"`
	Video      bool   `json:"video"`
	Season     int    `json:"season,omitempty"`
	Episode    int    `json:"episode,omitempty"`
}

// InspectResponse is the torrent's contents plus a guess at how it should be filed.
// Every guessed field is a suggestion for the form to prefill, never a decision:
// the caller still sends an explicit AddRequest.
type InspectResponse struct {
	Hash       string        `json:"hash"`
	Files      []InspectFile `json:"files"`
	Title      string        `json:"title"`
	Size       int64         `json:"size"`
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

// Inspect reports a torrent's files and classifies them. Metadata that never
// arrives is an error, not an empty list: "not ready" and "no files" are
// different answers.
//
// Filing decisions hinge on things only the torrent's contents reveal - whether
// it holds one episode or a whole season, which file is the feature rather than
// a sample - so the guess rides along for the video form. Audio callers read
// source_path and file_index and ignore the rest.
func (m *Manager) Inspect(ctx context.Context, req InspectRequest) (*InspectResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, errf(http.StatusRequestTimeout, "request cancelled: %v", err)
	}
	// Optional, unlike upstream: the engine names an untitled torrent from its
	// metadata, and the library browser inspects before it knows a title.
	title := strings.TrimSpace(req.Title)
	// Identity resolution mirrors validate(): a magnet's own info hash wins, so
	// cleanup later removes the torrent the engine was actually asked to add.
	hash, magnet, err := resolveAudioIdentity(req.Hash, req.Magnet)
	if err != nil {
		return nil, err
	}
	if magnet == "" {
		magnet = BuildMagnet(hash, title, DefaultTrackers())
	}

	// Held across ownership, add and cleanup, keyed on the canonical spelling so a
	// base32 magnet and its hex form are one torrent.
	lockKey := canonicalHashKey(hash)
	defer m.lockHash(lockKey)()

	known, ok := m.knownTorrentHashes(ctx)
	if !ok {
		// Adding without knowing what was already there would leak on failure:
		// there would be no way to tell whether this call hydrated the torrent.
		return nil, errf(http.StatusBadGateway, "cannot list torrents to establish ownership")
	}
	preexisting := known[lockKey]

	addedHash, err := m.cfg.GoStorm.AddTorrent(ctx, magnet, title)
	if err != nil || addedHash == "" {
		return nil, errf(http.StatusBadGateway, "gostorm rejected the torrent: %v", err)
	}
	engineHash := strings.ToLower(strings.TrimSpace(addedHash))
	if !reInfoHash.MatchString(engineHash) {
		// Hydrated under the hash we asked for, so that is the one to drop.
		if !preexisting {
			m.dropTorrent(ctx, hash)
		}
		return nil, errf(http.StatusBadGateway, "gostorm returned a malformed info hash %q", addedHash)
	}
	// A base32 magnet comes back in hex, and everything from here is keyed on the
	// spelling the engine reported, so the lock has to cover it too.
	if engineKey := canonicalHashKey(engineHash); engineKey != lockKey {
		defer m.lockHash(engineKey)()
		preexisting = preexisting || known[engineKey]
	}

	wait := req.MetadataWait
	if wait <= 0 {
		wait = defaultMetadataWait
	} else if wait > maxMetadataWait {
		wait = maxMetadataWait
	}
	info, err := m.cfg.GoStorm.GetTorrentInfo(ctx, engineHash, wait)
	if err != nil || info == nil {
		if !preexisting {
			m.dropTorrent(ctx, engineHash)
		}
		return nil, errf(http.StatusGatewayTimeout, "no metadata after %ds: %v", wait, err)
	}

	// Non-nil even when empty: an empty array and a null are different answers.
	out := &InspectResponse{Hash: engineHash, Title: info.Title, Size: info.Length, Files: make([]InspectFile, 0, len(info.FileStats))}

	var biggest InspectFile
	seasonSet := map[int]bool{}
	episodes := 0
	for _, f := range info.FileStats {
		name := filepath.Base(f.Path)
		season, episode := ParseSeasonEpisode(name)
		item := InspectFile{
			SourcePath: f.Path, FileIndex: f.ID, Name: name, Size: f.Length,
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
	out.Resolution = resolutionOf(info.Title)
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
		out.FileIndex = biggest.FileIndex
		out.Note = "single episode S" + pad2(out.Season) + "E" + pad2(out.Episode)

	default:
		// No episode numbering anywhere. A season marker in the torrent name
		// still means a pack whose files simply are not numbered.
		if mm := reSeasonOnly.FindStringSubmatch(info.Title); mm != nil && out.VideoCount > 1 {
			out.Kind, out.Pack = "tv", true
			out.Season = atoiSafe(mm[1])
			out.Seasons = []int{out.Season}
			out.Note = plural(out.VideoCount, "video file") +
				" and season " + itoa(out.Season) + " in the name — file as a season pack"
			break
		}
		out.Kind = "movie"
		out.FileIndex = biggest.FileIndex
		if out.VideoCount > 1 {
			out.Note = plural(out.VideoCount, "video file") +
				" with no episode numbering — largest picked (" + biggest.Name + ")"
		} else {
			out.Note = "single feature"
		}
	}
	return out, nil
}

// knownTorrentHashes snapshots what the engine already holds. A failed listing
// reports not-ok: the caller must not add without it.
func (m *Manager) knownTorrentHashes(ctx context.Context) (map[string]bool, bool) {
	torrents, err := m.cfg.GoStorm.ListTorrents(ctx)
	if err != nil {
		// Logged because this fails the whole request: without it the caller sees
		// only "cannot establish ownership" and the cause is invisible.
		m.cfg.Logger.Printf("[LibraryAPI] WARNING: cannot list torrents: %v", err)
		return nil, false
	}
	known := make(map[string]bool, len(torrents))
	for _, torrent := range torrents {
		known[canonicalHashKey(torrent.Hash)] = true
	}
	return known, true
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
