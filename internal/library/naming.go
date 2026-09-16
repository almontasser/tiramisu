// Package library holds the stub-file format and filename conventions shared by the
// sync engines and by the external /api/library endpoints. Both must agree: a stub
// created through the API under a different name would be a duplicate of the same
// title on disk, and a TV episode filed under a different registry key is deleted by
// the next sync as orphaned.
package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	reHDR   = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])hdr(?:$|[^A-Za-z0-9])|hdr10\+?`)
	reDV    = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])dv(?:$|[^A-Za-z0-9])|dovi|dolby.?vision`)
	reAtmos = regexp.MustCompile(`(?i)atmos`)
	re51    = regexp.MustCompile(`(?i)5\.1|dts|ddp5|ddp|dd\+|eac3|ac3`)
	// No separator required before the word: "BDRemux" and "UHDRemux" are common
	// spellings, and requiring one dropped the tag from names that announce it.
	reRemux     = regexp.MustCompile(`(?i)remux(?:$|[^A-Za-z0-9])`)
	reTitleYear = regexp.MustCompile(`(.+?)[._\s]\(?((?:19|20)\d{2})\)?`)

	reMovieUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)
	reUnderscores = regexp.MustCompile(`_+`)
	reShowUnsafe  = regexp.MustCompile(`[<>:"/\\|?*'"&]`)
	reSpaces      = regexp.MustCompile(`\s+`)
	reNonWord     = regexp.MustCompile(`[^a-z0-9]`)
	reEpSxxExx    = regexp.MustCompile(`[Ss](\d+)[Ee](\d+)`)
	reEpNxN       = regexp.MustCompile(`(\d+)x(\d+)`)
)

// MovieName is everything the movie filename is built from. ReleaseTitle is the raw
// release name the quality tags are read out of; Hash is the full info hash.
type MovieName struct {
	Title        string
	ReleaseDate  string
	ReleaseTitle string
	Is4K         bool
	Hash         string
}

func SanitizeMovieName(s string) string {
	s = reMovieUnsafe.ReplaceAllString(s, "_")
	s = reUnderscores.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// BuildMovieFilename returns the stub name for a movie. The trailing 8 chars are the
// tail of the info hash: that suffix is how an already-present title is recognised.
func BuildMovieFilename(n MovieName) string {
	year := ""
	if len(n.ReleaseDate) >= 4 {
		year = n.ReleaseDate[:4]
	} else if m := reTitleYear.FindStringSubmatch(n.Title); len(m) > 2 {
		year = m[2]
	}

	base := SanitizeMovieName(n.Title)
	if year != "" {
		base = fmt.Sprintf("%s_%s", base, year)
	}

	if n.Is4K {
		base += "_2160p"
	} else {
		base += "_1080p"
	}

	if reDV.MatchString(n.ReleaseTitle) {
		base += "_DV"
	} else if reHDR.MatchString(n.ReleaseTitle) {
		base += "_HDR"
	}

	if reAtmos.MatchString(n.ReleaseTitle) {
		base += "_Atmos"
	} else if re51.MatchString(n.ReleaseTitle) {
		base += "_5.1"
	}

	if reRemux.MatchString(n.ReleaseTitle) {
		base += "_REMUX"
	}

	return fmt.Sprintf("%s_%s.mkv", base, HashSuffix(n.Hash))
}

// HashSuffix is the last 8 hash chars used in movie filenames.
func HashSuffix(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[len(hash)-8:]
}

// HashPrefix is the first 8 hash chars used in episode filenames.
func HashPrefix(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}

// SanitizeShowName turns a show name into one safe directory component. Titles reach
// here from HTTP requests, so a name made of dots must not survive as "." or "..".
func SanitizeShowName(name string) string {
	clean := reShowUnsafe.ReplaceAllString(name, "")
	clean = reSpaces.ReplaceAllString(clean, "_")
	clean = reUnderscores.ReplaceAllString(clean, "_")
	clean = strings.Trim(clean, "_")
	clean = strings.TrimLeft(clean, ".")
	if clean == "" {
		return "untitled"
	}
	return clean
}

func ShowFolderName(showName, firstAirDate string) string {
	clean := SanitizeShowName(showName)
	if len(firstAirDate) >= 4 {
		return fmt.Sprintf("%s (%s)", clean, firstAirDate[:4])
	}
	return clean
}

func EpisodeFilename(show string, season, episode int, hash8 string) string {
	return fmt.Sprintf("%s_S%02dE%02d_%s.mkv", SanitizeShowName(show), season, episode, hash8)
}

// ShowDir is the show's folder under root. A folder that differs only in case is
// reused: TMDB names the show "One Piece" where the migration filed "ONE PIECE", and on
// a case-sensitive disk a second folder is a second series in Jellyfin.
func ShowDir(root, showName, firstAirDate string) string {
	name := ShowFolderName(showName, firstAirDate)
	found := name
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.Name() == name {
			return filepath.Join(root, name)
		}
		if e.IsDir() && strings.EqualFold(e.Name(), name) {
			found = e.Name()
		}
	}
	return filepath.Join(root, found)
}

// ShowKey is the show half of an episode's registry key: the show folder's name,
// lowercased, with everything but letters and digits removed. The folder carries the
// year, so a remake is not the original: keyed on the title alone, ONE PIECE (2023) and
// ONE PIECE (1999) shared "onepiece_s01e01", and filing either deleted the other's.
func ShowKey(show, firstAirDate string) string {
	return reNonWord.ReplaceAllString(strings.ToLower(ShowFolderName(show, firstAirDate)), "")
}

// EpisodeKey is the TV registry key. The sync deletes every stub under the TV dir whose
// path is not registered, so an API-created episode must use this exact form. metadb
// rebuilds the same form from a stub's folder when it migrates older keys.
func EpisodeKey(show, firstAirDate string, season, episode int) string {
	return fmt.Sprintf("%s_s%02de%02d", ShowKey(show, firstAirDate), season, episode)
}

// ParseSeasonEpisode reads S01E05 or 1x05 out of a filename, returning 0,0 when the
// name carries neither.
func ParseSeasonEpisode(filename string) (int, int) {
	for _, re := range []*regexp.Regexp{reEpSxxExx, reEpNxN} {
		if m := re.FindStringSubmatch(filename); len(m) >= 3 {
			season, _ := strconv.Atoi(m[1])
			episode, _ := strconv.Atoi(m[2])
			return season, episode
		}
	}
	return 0, 0
}

func IsVideoFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv", ".mp4", ".avi", ".mov", ".m4v":
		return true
	}
	return false
}

// reExtrasDir matches a directory of bonus material. Packs keep podcasts and
// featurettes there under names such as "S00E40 - Inside the Episode", which
// parse as episodes.
var reExtrasDir = regexp.MustCompile(`(?i)(^|/)(specials?|extras?|featurettes?|bonus|behind[ ._-]the[ ._-]scenes|deleted[ ._-]scenes|interviews?|trailers?|samples?)(/|$)`)

// IsExtrasPath reports whether a torrent file sits in a bonus-material directory.
func IsExtrasPath(path string) bool {
	return reExtrasDir.MatchString(filepath.ToSlash(filepath.Dir(path)))
}

var (
	reApostrophe = regexp.MustCompile(`['’]`)
	reNotAlnum   = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	reYear       = regexp.MustCompile(`^(19|20)\d{2}$`)
)

// MovieFile picks the file that holds a movie in a torrent's file list, or returns
// nil when the list does not show which one it is. A torrent can be a pack of
// films, and filing a pack's largest file filed 13 movies as other films from their
// packs: Interstellar played a film from "Imdb top 263 movies".
//
// Samples, trailers and bonus folders are never the feature. Of the rest, the file
// named for the movie wins; with no such file, a torrent holding one feature is that
// feature, and one large file beside small ones counts as one feature.
func MovieFile(files []FileStat, title string, year int) *FileStat {
	var features, named []*FileStat
	for i := range files {
		f := &files[i]
		if !IsVideoFile(f.Path) || IsExtrasPath(f.Path) || reJunk.MatchString(filepath.Base(f.Path)) {
			continue
		}
		features = append(features, f)
		if namesMovie(filepath.Base(f.Path), title, year) {
			named = append(named, f)
		}
	}
	if len(named) > 0 {
		return onlyFeature(named)
	}
	return onlyFeature(features)
}

// onlyFeature returns the one file, or the largest when every other file is under
// 1 GiB and a fifth of its size. Anything else is several films.
func onlyFeature(files []*FileStat) *FileStat {
	if len(files) == 0 {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Length > files[j].Length })
	for _, f := range files[1:] {
		if f.Length >= 1<<30 || f.Length*5 >= files[0].Length {
			return nil
		}
	}
	return files[0]
}

// namesMovie reports whether a file name carries the title, either as words or with
// the separators squeezed out ("Spiderman 3" for "Spider-Man 3"), and no year but the
// movie's: "The Dark Knight Rises (2012)" also carries the words of The Dark Knight.
func namesMovie(name, title string, year int) bool {
	nameWords := words(strings.TrimSuffix(name, filepath.Ext(name)))
	titleWords := words(title)
	if len(titleWords) == 0 {
		return false
	}
	if year > 0 {
		for _, w := range nameWords {
			if reYear.MatchString(w) && w != strconv.Itoa(year) {
				return false
			}
		}
	}
	have := map[string]bool{}
	for _, w := range nameWords {
		have[w] = true
	}
	all := true
	for _, w := range titleWords {
		all = all && have[w]
	}
	// Squeezing a short title matches inside unrelated words: "up" is in "1080p.bluray.x264-SUPERB".
	squeezed := strings.Join(titleWords, "")
	return all || (len(squeezed) >= 8 && strings.Contains(strings.Join(nameWords, ""), squeezed))
}

// words lowercases s and splits it on anything but letters and digits, dropping
// apostrophes first so that "World's" and "Worlds" agree.
func words(s string) []string {
	var out []string
	for _, w := range reNotAlnum.Split(reApostrophe.ReplaceAllString(strings.ToLower(s), ""), -1) {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

// StubChanged is called with the path of every stub or stub directory written or
// removed, so the media server can refresh just that folder. WriteStub calls it; the
// removals call it from main, where they go through the FUSE layer's invalidation.
// main sets it before anything writes a stub.
var StubChanged = func(path string) {}

// WriteStub writes the virtual .mkv: a small JSON file the FUSE layer exposes at the
// declared size.
func WriteStub(path, streamURL string, size int64, magnet, imdbID string) error {
	data, err := json.Marshal(map[string]interface{}{
		"url":    streamURL,
		"size":   size,
		"magnet": magnet,
		"imdb":   imdbID,
	})
	if err != nil {
		return err
	}
	if err := mkdirAllOwned(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}
	chownPath(path)
	StubChanged(path)
	return nil
}
