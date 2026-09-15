package engines

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"tiramisu/internal/catalog/mediaserver"
	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/catalog/torrentio"
	"tiramisu/internal/config"
	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
	"tiramisu/internal/prowlarr"
)

// TVGoEngine is the pure Go implementation of TV sync.
type TVGoEngine struct {
	gostorm   *GoStormClient
	tmdb      *tmdb.Client
	torrentio *torrentio.Client
	prowlarr  *prowlarr.Client
	plexURL   string
	plexToken string
	plexTVLib int
	mediasrv  mediaserver.Client
	tvDir     string
	// animeDir is the anime tree, a sibling of tvDir. Empty disables the split
	// and every series is filed under tvDir, which is upstream's behaviour.
	animeDir string
	stateDir string
	limiter  *rate.Limiter
	logger   *log.Logger

	registry     map[string]TVEpisodeEntry
	registryFile string
	db           *metadb.DB // V1.7.1: Optional SQLite backend

	processedThisRun map[string]bool
	stats            TVSyncStats

	// name is the scheduler job this engine runs as, and mode the series its
	// runs cover (see TVSyncMode). Nil mode covers every series.
	name string
	mode func() TVSyncMode
	// deadEpisodeKeys holds the episodes whose torrent stopped resolving its metadata.
	// They stay in the registry - deregistering would have cleanupOrphanedFiles delete
	// their stubs at the end of the run - but every quality comparison must treat them
	// as absent, or a replacement of equal quality is refused and the dead release
	// keeps its place. Rebuilt every run.
	deadEpisodeKeys map[string]bool
	// deadPackHashes keeps the releases already known to be silent out of the
	// candidate list: re-selecting one costs a full metadata wait for nothing.
	deadPackHashes map[string]bool

	blacklist     BlacklistData
	blacklistFile string

	invalidatePath func(string)

	reITA         *regexp.Regexp
	reExclLang    *regexp.Regexp
	exclLanguages map[string]bool

	weights config.TVWeights

	maxSeasons int

	// Anime routing. A show passing every configured test is filed under
	// animeDir rather than tvDir. The tests are ANDed on purpose: TMDB genre 16
	// alone is "Animation", which is as true of The Simpsons as of Cowboy Bebop.
	animeEnabled   bool
	animeGenreIDs  map[int]bool
	animeLanguages map[string]bool

	// knownTitles caches a show's TMDB aliases for the process lifetime, not per
	// run: aliases change rarely, and refetching ~100 shows every run would cost
	// ~25s of rate-limited calls. A show met for the first time is always fetched.
	knownTitles map[int][]string

	// tmdbLangs are the languages a show's localized name is fetched in.
	tmdbLangs []string
}

// TVEpisodeEntry is a single entry in the TV episode registry.
type TVEpisodeEntry struct {
	QualityScore int    `json:"quality_score"`
	Hash         string `json:"hash"`
	FilePath     string `json:"file_path"`
	Source       string `json:"source"`
	Created      int64  `json:"created"`
}

// TVSyncStats tracks sync statistics.
type TVSyncStats struct {
	Shows           int `json:"shows"`
	EpisodesCreated int `json:"episodes_created"`
	EpisodesSkipped int `json:"episodes_skipped"`
	Upgrades        int `json:"upgrades"`
}

// TVEngineConfig holds config for the TV engine.
type TVEngineConfig struct {
	// Name is the scheduler job, and names the log file.
	Name string
	// Mode chooses the series a run covers; nil covers every series.
	Mode            func() TVSyncMode
	GoStormURL      string
	TMDBAPIKey      string
	TorrentioURL    string
	PlexURL         string
	PlexToken       string
	MediaServerType string
	PlexTVLib       int
	TVDir           string
	// AnimeDir is the anime tree. Empty means no split: every series is filed
	// under TVDir, exactly as upstream does.
	AnimeDir       string
	AnimeEnabled   bool
	AnimeGenreIDs  []int
	AnimeLanguages []string
	StateDir       string
	LogsDir        string
	ProwlarrCfg    prowlarr.ConfigProwlarr
	// InvalidatePath, when set, is called after removing a stub file/dir so the FUSE
	// layer drops its cached state for it (see main.invalidateSyncRemovedPath).
	InvalidatePath func(string)
	Language       config.LanguageConfig
	Weights        config.TVWeights
	// MaxSeasons caps how many of the most recent seasons are considered.
	// <= 0 means every season.
	MaxSeasons int
}

// TV thresholds
const (
	tvMinEpisodeSize = 1073741824  // 1GB
	tvMaxEpisodeSize = 32212254720 // 30GB
	// tvShowMatchEnforce turns the contents check from reporting into rejecting.
	// Enabled 2026-08-21 after a full 100-show run: 19 torrents inspected, zero
	// false rejects. A torrent that reaches here has already been downloaded, so
	// rejecting costs nothing but the discard.
	tvShowMatchEnforce = true

	tvUpgradeThreshold = 1.2
	tvSinglesLimit     = 15
	tvMaxShowAgeDays   = 180
)

var (
	reTV4K    = regexp.MustCompile(`(?i)2160p|4k|uhd`)
	reTV1080p = regexp.MustCompile(`(?i)1080p`)
	// \b treats "_" as a word char, so "\bhdr\b" misses "_HDR_" - use a custom boundary.
	reTVHDR          = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])hdr(?:$|[^A-Za-z0-9])|hdr10\+?`)
	reTVDV           = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])dv(?:$|[^A-Za-z0-9])|dovi|dolby.?vision`)
	reTVAtmos        = regexp.MustCompile(`(?i)atmos`)
	reTV51           = regexp.MustCompile(`(?i)5\.1|dd5|ddp5|dts|truehd`)
	reTVSeeders      = regexp.MustCompile(`👤\s*(\d+)`)
	reTVFullpack     = regexp.MustCompile(`(?i)\b(season|complete|full|pack)\b`)
	reTVRange        = regexp.MustCompile(`(?i)s\d+e\d+\s*-\s*e?\d+`)
	reTVMultiEp      = regexp.MustCompile(`(?i)s\d+e\d+`)
	reTVSeason       = regexp.MustCompile(`\.s\d{2}\.`)
	reTVSeasonP      = regexp.MustCompile(`\ss\d{2}\s*\(`)
	reTVSpecialTitle = regexp.MustCompile(`(?i)\b(special|christmas|bonus|extra|ova)\b`)
	reTVSeasonN      = regexp.MustCompile(`[Ss](\d+)`)
	reTVSeasonR      = regexp.MustCompile(`\bs(\d{1,2})\s*[-–]\s*s(\d{1,2})\b`)
	reTVSeasonW      = regexp.MustCompile(`\bseasons?\s*(\d{1,2})\s*[-–]\s*(\d{1,2})\b`)
	reTVCompleteS    = regexp.MustCompile(`(?i)\b(complete\s+series|all\s+seasons|full\s+series)\b`)
	reTVEpNum        = regexp.MustCompile(`[Ss](\d+)[Ee](\d+)`)
	reTVFileName     = regexp.MustCompile(`(?i)(.+)_S(\d+)E(\d+)_([a-f0-9]{8})\.mkv$`)
	reTVNonWord      = regexp.MustCompile(`[^a-z0-9]`)
	reTVYear         = regexp.MustCompile(`\(?(\d{4})\)?`)
	reTVQuality      = regexp.MustCompile(`\b(2160p|1080p|720p|4k|uhd|hdr|dv|dovi|web|bluray|remux)\b.*`)
	reTVHashURL      = regexp.MustCompile(`(?i)link=([a-f0-9]{40})`)
)

var tvExcludedGenreIDs = map[int]bool{99: true, 10763: true, 10764: true, 10767: true, 16: true}

// tmdbGenreAnimation is TMDB's "Animation". It sits in the excluded set above
// because upstream has nowhere to file anime — every series lands in tv/ — not
// because animation is unwanted. An anime tree is what makes it admissible.
const tmdbGenreAnimation = 16

// isAnimeShow reports whether a show belongs in the anime tree. Every configured
// test must pass: the genre alone is "Animation", which is as true of The
// Simpsons as of Cowboy Bebop, so the original language is what separates the
// two. An empty rule set means that test is not applied.
func (e *TVGoEngine) isAnimeShow(show tmdb.TVShow) bool {
	if !e.animeEnabled {
		return false
	}
	if len(e.animeLanguages) > 0 && !e.animeLanguages[strings.ToLower(show.Language)] {
		return false
	}
	if len(e.animeGenreIDs) == 0 {
		return true
	}
	for _, gid := range show.GenreIDs {
		if e.animeGenreIDs[gid] {
			return true
		}
	}
	return false
}

// showDir is the tree a show's stubs belong in.
func (e *TVGoEngine) showDir(show tmdb.TVShow) string {
	if e.isAnimeShow(show) {
		return e.animeDir
	}
	return e.tvDir
}

// roots is every tree this engine writes into. Walks that reconcile the
// registry against disk must cover all of them: one that walked only tvDir
// would find every anime stub unregistered and delete it as orphaned.
func (e *TVGoEngine) roots() []string {
	if e.animeEnabled && e.animeDir != "" && e.animeDir != e.tvDir {
		return []string{e.tvDir, e.animeDir}
	}
	return []string{e.tvDir}
}

// walkRoots walks every tree the engine writes into, skipping any that does not
// exist yet. Every reconciliation pass must go through this rather than walking
// tvDir directly: cleanupOrphanedFiles deletes each .mkv it finds that is not in
// the registry, so a walk that missed the anime tree would be harmless, but one
// that missed it while the registry held its episodes would not — and
// populateRegistryFromExisting would silently rebuild a registry with no anime
// in it at all.
func (e *TVGoEngine) walkRoots(fn filepath.WalkFunc) {
	for _, root := range e.roots() {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		filepath.Walk(root, fn)
	}
}

// isRoot reports whether path is one of the trees themselves, as opposed to
// something inside one. The empty-directory sweep uses it so it never removes a
// tree root that happens to be empty.
func (e *TVGoEngine) isRoot(path string) bool {
	for _, r := range e.roots() {
		if path == r {
			return true
		}
	}
	return false
}

// NewTVGoEngine creates a new Go TV sync engine.
func NewTVGoEngine(cfg TVEngineConfig, db *metadb.DB) *TVGoEngine {
	var prowlarrClient *prowlarr.Client
	if cfg.ProwlarrCfg.Enabled {
		prowlarrClient = prowlarr.NewClient(cfg.ProwlarrCfg)
	}

	// Each job logs to its own file: tv-sync.log, anime-sync.log.
	name := cfg.Name
	if name == "" {
		name = "tv"
	}
	prefix := "[TVSync] "
	if name != "tv" {
		prefix = "[" + strings.ToUpper(name[:1]) + name[1:] + "Sync] "
	}
	logPath := filepath.Join(cfg.LogsDir, name+"-sync.log")
	logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	logger := log.New(io.MultiWriter(os.Stdout, logFile), prefix, log.LstdFlags)

	regFile := filepath.Join(cfg.StateDir, "tv_episode_registry.json")
	blFile := filepath.Join(cfg.StateDir, "blacklist.json")

	// Anime routing rules, resolved once. The split stays off unless a tree is
	// configured: with no AnimeDir there is nowhere to file to.
	animeGenres := make(map[int]bool, len(cfg.AnimeGenreIDs))
	for _, g := range cfg.AnimeGenreIDs {
		animeGenres[g] = true
	}
	animeLangs := make(map[string]bool, len(cfg.AnimeLanguages))
	for _, l := range cfg.AnimeLanguages {
		animeLangs[strings.ToLower(strings.TrimSpace(l))] = true
	}

	e := &TVGoEngine{
		gostorm:          NewGoStormClient(cfg.GoStormURL),
		tmdb:             tmdb.NewClient(cfg.TMDBAPIKey),
		torrentio:        torrentio.NewClient(cfg.TorrentioURL, "sort=qualitysize|qualityfilter=480p,720p,scr,cam"),
		prowlarr:         prowlarrClient,
		plexURL:          cfg.PlexURL,
		plexToken:        cfg.PlexToken,
		plexTVLib:        cfg.PlexTVLib,
		mediasrv:         mediaserver.New(cfg.MediaServerType, cfg.PlexURL, cfg.PlexToken),
		tvDir:            cfg.TVDir,
		animeDir:         cfg.AnimeDir,
		animeEnabled:     cfg.AnimeEnabled && cfg.AnimeDir != "",
		animeGenreIDs:    animeGenres,
		animeLanguages:   animeLangs,
		stateDir:         cfg.StateDir,
		limiter:          rate.NewLimiter(rate.Every(500*time.Millisecond), 1),
		logger:           logger,
		registryFile:     regFile,
		db:               db,
		processedThisRun: make(map[string]bool),
		knownTitles:      make(map[int][]string),
		blacklistFile:    blFile,
		invalidatePath:   cfg.InvalidatePath,
		reITA:            CompileLanguageRegex(cfg.Language.PreferredTerms, cfg.Language.PreferredFlags),
		reExclLang:       CompileLanguageRegex(ExcludedTitleTerms(cfg.Language.ExcludedFlags), cfg.Language.ExcludedFlags),
		exclLanguages:    ExcludedLanguageSet(cfg.Language.ExcludedFlags),
		weights:          cfg.Weights,
		maxSeasons:       cfg.MaxSeasons,
		tmdbLangs:        TMDBEpisodeLanguages(cfg.Language.PreferredFlags),
	}

	e.registry = e.loadRegistry()
	e.blacklist = e.loadBlacklist()

	return e
}

// removeStub deletes a stub file/dir, invalidates its FUSE cache state, and removes the
// underlying torrent from GoStorm. hash may be empty (e.g. for a plain directory); a
// failed RemoveTorrent doesn't block the stub deletion.
func (e *TVGoEngine) removeStub(ctx context.Context, path, hash string) {
	if hash != "" {
		if err := e.gostorm.RemoveTorrent(ctx, hash); err != nil {
			e.logger.Printf("[TVSync] WARNING: failed to remove torrent %s for %s: %v", hash, filepath.Base(path), err)
		}
	}
	e.removeStubFile(path)
}

// removeStubFile drops the stub without touching the engine, for callers that share one
// torrent across several stubs and drop it once.
func (e *TVGoEngine) removeStubFile(path string) {
	os.Remove(path)
	if e.invalidatePath != nil {
		e.invalidatePath(path)
	}
}

func (e *TVGoEngine) loadBlacklist() BlacklistData {
	data, err := os.ReadFile(e.blacklistFile)
	if err != nil {
		return BlacklistData{Hashes: make(map[string]string), Titles: []string{}}
	}
	var bl BlacklistData
	json.Unmarshal(data, &bl)
	if bl.Hashes == nil {
		bl.Hashes = make(map[string]string)
	}
	return bl
}

func (e *TVGoEngine) normalizeTitle(title string) string {
	t := strings.ToLower(title)
	t = reTVYear.ReplaceAllString(t, "")
	t = reTVQuality.ReplaceAllString(t, "")
	t = reTVNonWord.ReplaceAllString(t, "")
	return t
}

func (e *TVGoEngine) isBlacklisted(title string) bool {
	normalized := e.normalizeTitle(title)
	for _, bt := range e.blacklist.Titles {
		if bt == normalized {
			return true
		}
	}
	return false
}

// isDeadPackHash reports a release this run already found silent.
func (e *TVGoEngine) isDeadPackHash(hash string) bool {
	return e.deadPackHashes[strings.ToLower(hash)]
}

func (e *TVGoEngine) isHashBlacklisted(hash string) bool {
	_, ok := e.blacklist.Hashes[strings.ToLower(hash)]
	return ok
}

func (e *TVGoEngine) Name() string { return "tv" }

func (e *TVGoEngine) Run(ctx context.Context) error {
	e.logger.Printf("Starting TV sync...")
	// B1.1: reset per-run state so repeated scheduler invocations start clean.
	// processedThisRun and stats are long-lived struct fields, not local vars.
	e.processedThisRun = make(map[string]bool)
	e.stats = TVSyncStats{}
	// The Library API writes episodes to the StateDB while this engine keeps
	// the copy it loaded at startup. Working from that copy, reconcile drops
	// the row of an episode the API re-filed under a new path, because the
	// old path is gone, and cleanup then deletes the new stub as an orphan.
	// Start every run from the database, and from the current blacklist.
	e.registry = e.loadRegistry()
	e.blacklist = e.loadBlacklist()
	e.deadEpisodeKeys = make(map[string]bool)
	e.deadPackHashes = make(map[string]bool)
	e.populateRegistryFromExisting()
	e.reconcileRegistry()

	// Flagged before discovery: the reaper does not depend on it, and a TMDB outage or
	// a filter that empties the list must not leave dead packs untouched for days.
	deadPacks := e.flagDeadPacks()

	shows, err := e.discoverShows(ctx)
	if err != nil {
		e.logger.Printf("Discover error: %v", err)
		e.reapDeadPacks(ctx, deadPacks)
		return fmt.Errorf("discover shows: %w", err)
	}
	if len(shows) == 0 {
		e.logger.Printf("No shows discovered")
		e.reapDeadPacks(ctx, deadPacks)
		return nil
	}
	e.logger.Printf("Discovered %d shows", len(shows))

	for i, show := range shows {
		select {
		case <-ctx.Done():
			e.logger.Printf("Stopped after %d/%d shows (%d created)", i, len(shows), e.stats.EpisodesCreated)
			return ctx.Err()
		default:
		}

		e.logger.Printf("[%d/%d] %s", i+1, len(shows), show.Name)
		e.processShow(ctx, show)
	}

	// After the loop: a pack that died on an old season is outside the discovery
	// window, so this is the only pass that reaches it. Before the cleanup, which
	// deletes anything the registry no longer lists.
	e.reapDeadPacks(ctx, deadPacks)
	// After the reaper. A hole opened in this run is skipped here: it was just created
	// by a search that failed on the same season, so retrying it now would cost a full
	// season search to learn nothing.
	e.repairEpisodeGaps(ctx)

	// Only write JSON registry when DB is unavailable. If DB is active,
	// episodes were already persisted via registerEpisode() → UpsertEpisode().
	// Writing JSON here would recreate tv_episode_registry.json after migration,
	// causing the crash-recovery logic to wipe the entire DB on next restart.
	if e.db == nil {
		e.saveRegistry()
	}
	// B6.1: rehydrate before cleanup — if an MKV file exists on disk but its
	// torrent is missing from GoStorm, restore it first. cleanupOrphanedFiles
	// runs after so it cannot delete a file that rehydrate still needs.
	e.rehydrateMissingTorrents(ctx)
	if e.mergeRegistryFromDB() {
		e.cleanupOrphanedFiles(ctx)
		e.cleanupOrphanedTorrents(ctx)
	}

	e.logger.Printf("TV sync complete: %d shows, %d episodes created, %d skipped, %d upgrades",
		e.stats.Shows, e.stats.EpisodesCreated, e.stats.EpisodesSkipped, e.stats.Upgrades)

	// Notify the media server. Plex skips this without a section ID; Jellyfin skips
	// it too, because each stub written or removed was already reported to it.
	if e.stats.EpisodesCreated > 0 {
		if err := e.mediasrv.RefreshLibrary(context.Background(), e.plexTVLib); err != nil {
			e.logger.Printf("Warning: media server library refresh failed: %v", err)
		}
	}

	return nil
}

// mergeRegistryFromDB reads into the registry the episodes filed since the run
// loaded it, and reports whether cleanup is safe. Cleanup deletes every stub the
// registry does not list, and the Seerr bridge and the hourly episode job file
// episodes through the Library API while a run of several hours is working: from
// the copy loaded at the start, cleanup would delete all of them. Database rows
// win over this run's copy; entries only the copy holds, such as the stubs
// adopted at the start of the run, stay. Without a readable database, cleanup is
// skipped rather than run blind.
func (e *TVGoEngine) mergeRegistryFromDB() bool {
	if e.db == nil {
		return true
	}
	entries, err := e.db.AllEpisodes()
	if err != nil {
		e.logger.Printf("Warning: cannot reread the registry, skipping cleanup: %v", err)
		return false
	}
	for _, entry := range entries {
		e.registry[entry.EpisodeKey] = TVEpisodeEntry{
			QualityScore: entry.QualityScore,
			Hash:         entry.Hash,
			FilePath:     entry.FilePath,
			Source:       entry.Source,
			Created:      entry.Created,
		}
	}
	return true
}

func (e *TVGoEngine) loadRegistry() map[string]TVEpisodeEntry {
	if e.db != nil {
		entries, err := e.db.AllEpisodes()
		if err != nil {
			e.logger.Printf("[TVSync] Warning: failed to load registry from DB: %v", err)
		} else {
			reg := make(map[string]TVEpisodeEntry)
			for _, entry := range entries {
				reg[entry.EpisodeKey] = TVEpisodeEntry{
					QualityScore: protectedScore(entry.QualityScore, entry.Source),
					Hash:         entry.Hash,
					FilePath:     entry.FilePath,
					Source:       entry.Source,
					Created:      entry.Created,
				}
			}
			e.logger.Printf("[TVSync] Loaded %d episodes from StateDB", len(reg))
			return reg
		}
	}
	data, err := os.ReadFile(e.registryFile)
	if err != nil {
		return make(map[string]TVEpisodeEntry)
	}
	var reg map[string]TVEpisodeEntry
	if err := json.Unmarshal(data, &reg); err != nil {
		return make(map[string]TVEpisodeEntry)
	}
	return reg
}

func (e *TVGoEngine) saveRegistry() {
	data, err := json.MarshalIndent(e.registry, "", "  ")
	if err != nil {
		return
	}
	tmp := e.registryFile + ".tmp"
	os.WriteFile(tmp, data, 0644)
	os.Rename(tmp, e.registryFile)
}

func (e *TVGoEngine) populateRegistryFromExisting() {
	var torrents []TorrentStats
	var tsLoaded bool

	e.walkRoots(func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}

		if _, ok := e.registryByPath(path); ok {
			return nil
		}

		filename := filepath.Base(path)
		m := reTVFileName.FindStringSubmatch(filename)
		if len(m) < 5 {
			return nil
		}

		showName := m[1]
		season, _ := strconv.Atoi(m[2])
		episode, _ := strconv.Atoi(m[3])
		hash8 := strings.ToLower(m[4])
		key := e.episodeKey(showName, season, episode)

		if _, exists := e.registry[key]; exists {
			return nil
		}

		// Try to read full hash from file content first
		fullHash := e.readHashFromMKV(path)
		if fullHash == "" {
			// Fallback: resolve via GoStorm lookup
			if !tsLoaded {
				torrents, _ = e.gostorm.ListTorrents(context.Background())
				tsLoaded = true
			}
			for _, t := range torrents {
				if strings.HasPrefix(strings.ToLower(t.Hash), hash8) {
					fullHash = strings.ToLower(t.Hash)
					break
				}
			}
			if fullHash == "" {
				fullHash = hash8
			}
		}

		e.registry[key] = TVEpisodeEntry{
			QualityScore: 1,
			Hash:         fullHash,
			FilePath:     path,
			Source:       "existing",
			Created:      info.ModTime().Unix(),
		}

		return nil
	})
}

func (e *TVGoEngine) readHashFromMKV(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 10240 {
		return ""
	}
	content := strings.TrimSpace(string(data))
	if !strings.HasPrefix(content, "{") {
		return ""
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return ""
	}
	url, _ := obj["url"].(string)
	m := reTVHashURL.FindStringSubmatch(url)
	if len(m) > 1 {
		return strings.ToLower(m[1])
	}
	return ""
}

func (e *TVGoEngine) registryByPath(path string) (string, bool) {
	for key, entry := range e.registry {
		if entry.FilePath == path {
			return key, true
		}
	}
	return "", false
}

// isDeadEpisode reports whether an episode's release stopped answering. A dead entry
// must not win a quality comparison: it is still on disk, but it cannot be played.
func (e *TVGoEngine) isDeadEpisode(key string) bool {
	return e.deadEpisodeKeys[key]
}

func (e *TVGoEngine) episodeKey(show string, season, episode int) string {
	return library.EpisodeKey(show, season, episode)
}

func (e *TVGoEngine) registerEpisode(key string, score int, hash, path, source, showIMDB string) {
	created := time.Now().Unix()
	// The episode has a live release again: it must stop being treated as absent in
	// the comparisons for the rest of the run.
	delete(e.deadEpisodeKeys, key)
	e.registry[key] = TVEpisodeEntry{
		QualityScore: score,
		Hash:         hash,
		FilePath:     path,
		Source:       source,
		Created:      created,
	}
	if e.db != nil {
		if err := e.db.UpsertEpisode(key, metadb.EpisodeEntry{
			EpisodeKey:   key,
			QualityScore: score,
			Hash:         hash,
			FilePath:     path,
			Source:       source,
			Created:      created,
			ShowIMDB:     showIMDB,
		}); err != nil {
			e.logger.Printf("[TVSync] Warning: failed to save episode to DB: %v", err)
		}
		// The hole is filled: close it here rather than only in the repair pass, or a
		// client that reads the gap list and adds the episode itself keeps seeing it.
		if err := e.db.ClearEpisodeGap(key); err != nil {
			e.logger.Printf("[TVSync] Warning: could not clear the gap for %s: %v", key, err)
		}
	} else {
		e.saveRegistry()
	}
}

func (e *TVGoEngine) discoverShows(ctx context.Context) ([]tmdb.TVShow, error) {
	cutoff := time.Now().AddDate(0, 0, -tvMaxShowAgeDays).Format("2006-01-02")

	var all []tmdb.TVShow
	seen := make(map[int]bool)

	endpoints := []struct {
		fn    func(context.Context, int) ([]tmdb.TVShow, error)
		pages int
	}{
		{e.tmdb.TVOnTheAir, 3},
		{e.tmdb.TVAiringToday, 2},
		{e.tmdb.TVTrending, 3},
	}

	for _, ep := range endpoints {
		shows, err := ep.fn(ctx, ep.pages)
		if err != nil {
			continue
		}
		for _, s := range shows {
			if !seen[s.ID] && e.wants(s) && e.passesShowFilters(s) {
				seen[s.ID] = true
				all = append(all, s)
			}
		}
	}

	// Discover English recent shows
	discShows, err := e.tmdb.DiscoverTV(ctx, "en", cutoff, "", 5)
	if err == nil {
		for _, s := range discShows {
			if !seen[s.ID] && e.wants(s) && e.passesShowFilters(s) {
				seen[s.ID] = true
				all = append(all, s)
			}
		}
	}

	return all, nil
}

// isShowRecent returns true if the show has had recent activity within tvMaxShowAgeDays.
// Mirrors Python's _is_show_recent(): checks first_air_date, last_air_date, and
// next_episode_to_air so that old shows with new seasons (e.g. Stranger Things S5)
// are not incorrectly filtered out.
func isShowRecent(details *tmdb.TVDetail) bool {
	cutoff := time.Now().AddDate(0, 0, -tvMaxShowAgeDays)
	parse := func(s string) (time.Time, bool) {
		t, err := time.Parse("2006-01-02", s)
		return t, err == nil && !t.IsZero()
	}

	if t, ok := parse(details.FirstAirDate); ok && t.After(cutoff) {
		return true
	}
	if t, ok := parse(details.LastAirDate); ok && t.After(cutoff) {
		return true
	}
	if details.NextEpisodeToAir != nil {
		return true
	}
	return false
}

func (e *TVGoEngine) passesShowFilters(show tmdb.TVShow) bool {
	// Genre filter. Animation is exempt for a show this engine would file as
	// anime: excluding it is only right while there is nowhere to put it, and
	// with an anime tree configured the exclusion would keep that library
	// permanently empty. Documentary, news, talk and reality stay excluded
	// either way.
	isAnime := e.isAnimeShow(show)
	for _, gid := range show.GenreIDs {
		if !tvExcludedGenreIDs[gid] {
			continue
		}
		if isAnime && gid == tmdbGenreAnimation {
			continue
		}
		return false
	}

	// Language: explicitly excluded languages are a hard reject regardless of
	// provider availability; English is always accepted; other languages need
	// a premium IT provider.
	if e.exclLanguages[show.Language] {
		return false
	}
	if show.Language == "en" {
		return true
	}

	details, err := e.tmdb.TVDetails(context.Background(), show.ID)
	if err != nil {
		return false
	}

	return tmdb.HasPremiumProvider(details.WatchProviders)
}

// processShow searches one show and acquires what it is missing. seasons, when given,
// replaces the default window of the last two: the reaper needs the seasons a dead
// release covers. The returned map says which of them were conclusively searched.
func (e *TVGoEngine) processShow(ctx context.Context, show tmdb.TVShow, seasons ...int) map[int]seasonSearch {
	showName := show.Name
	if showName == "" {
		showName = show.OriginalName
	}
	if showName == "" {
		return nil
	}

	// Blacklist check at show level
	if e.isBlacklisted(showName) {
		e.logger.Printf("🚫 Blacklist: skipping show '%s'", showName)
		return nil
	}

	t0 := time.Now()
	imdbID, err := e.tmdb.TVExternalIDs(ctx, show.ID)
	if err != nil || imdbID == "" {
		return nil
	}

	details, err := e.tmdb.TVDetails(ctx, show.ID)
	if err != nil {
		return nil
	}
	e.logger.Printf("  TMDB lookups: %v", time.Since(t0).Round(time.Millisecond))

	// Age check: skip shows with no recent activity (mirrors Python _is_show_recent).
	// TVOnTheAir/AiringToday endpoints guarantee recency, but TVTrending and Discover
	// can surface old shows. We check all three conditions on TVDetail fields:
	//   1. premiered recently (first_air_date)
	//   2. aired recently — catches old shows with new seasons (last_air_date)
	//   3. has upcoming episodes planned (next_episode_to_air)
	// The recency gate keeps the routine sync on shows still airing. The reaper asks
	// for explicit seasons, and it asks because a release there is already dead: a
	// show off the air is exactly the case it exists for.
	if len(seasons) == 0 && !isShowRecent(details) {
		e.logger.Printf("  Skipping '%s' — no recent activity (last: %s, next: %v)", showName, details.LastAirDate, details.NextEpisodeToAir != nil)
		return nil
	}

	// Check complete seasons
	completeSeasons := e.getCompleteSeasons(showName, details)
	skippedSeasons := make(map[int]bool)

	// Determine target seasons range
	numSeasons := details.NumberOfSeasons
	if numSeasons == 0 {
		numSeasons = 5
	}
	maxSeasons := e.seasonWindow(numSeasons)
	startSeason := numSeasons - maxSeasons + 1
	if startSeason < 1 {
		startSeason = 1
	}
	endSeason := numSeasons
	if len(seasons) > 0 {
		startSeason, endSeason = seasons[0], seasons[0]
		for _, sn := range seasons {
			if sn < startSeason {
				startSeason = sn
			}
			if sn > endSeason {
				endSeason = sn
			}
		}
	}

	allTargetComplete := true
	for s := startSeason; s <= endSeason; s++ {
		if avgScore, ok := completeSeasons[s]; ok && avgScore >= float64(e.weights.SeasonSkipScore) {
			skippedSeasons[s] = true
		} else {
			allTargetComplete = false
		}
	}

	// If ALL target seasons are complete, skip entire show immediately
	if allTargetComplete {
		e.logger.Printf("Skipping '%s' — all %d target seasons complete", showName, endSeason-startSeason+1)
		return nil
	}

	// Get streams
	t1 := time.Now()
	streams, searched := e.getStreams(ctx, imdbID, show.ID, showName, details, seasons...)
	e.logger.Printf("  getStreams: %v (%d streams)", time.Since(t1).Round(time.Millisecond), len(streams))
	if len(streams) == 0 {
		return searched
	}

	sort.SliceStable(streams, func(i, j int) bool {
		return streams[i].Priority > streams[j].Priority
	})

	created := 0
	seasonsComplete := make(map[int]bool)
	seasonsEpCount := make(map[int]int)

	tmdbSeasonEps := make(map[int]int)
	for _, sd := range details.Seasons {
		if sd.SeasonNumber > 0 && sd.EpisodeCount > 0 {
			tmdbSeasonEps[sd.SeasonNumber] = sd.EpisodeCount
		}
	}

	knownTitles := e.showKnownTitles(ctx, show.ID, details)

	// Process fullpacks first
	fpCount := 0
	for _, s := range streams {
		if s.IsFullpack {
			fpCount++
		}
	}
	e.logger.Printf("  %d fullpacks, %d singles, skippedSeasons=%v", fpCount, len(streams)-fpCount, skippedSeasons)

	for _, stream := range streams {
		if !stream.IsFullpack {
			continue
		}
		if skippedSeasons[stream.Season] {
			continue
		}
		if seasonsComplete[stream.Season] {
			continue
		}
		// Pre-check: if season is already complete in registry at equal/lower quality, skip
		// without fetching torrent info (AddTorrent + GetTorrentInfo can take up to 90s).
		if avgScore, isComplete := completeSeasons[stream.Season]; isComplete {
			if float64(stream.QualityScore) <= avgScore*tvUpgradeThreshold {
				e.logger.Printf("    fullpack S%02d: already complete (registry avg=%.0f, stream=%d) — skipped", stream.Season, avgScore, stream.QualityScore)
				continue
			}
		}

		t2 := time.Now()
		count := e.processFullpack(ctx, showName, imdbID, e.showDir(show), stream, show.FirstAirDate, knownTitles)
		e.logger.Printf("    fullpack S%02d: %d created in %v (%s)", stream.Season, count, time.Since(t2).Round(time.Millisecond), stream.Title[:min(60, len(stream.Title))])
		if count > 0 {
			created += count
			seasonsEpCount[stream.Season] += count
			expected := tmdbSeasonEps[stream.Season]
			total := seasonsEpCount[stream.Season]
			if expected > 0 && total >= expected {
				seasonsComplete[stream.Season] = true
			} else if !stream.IsPartialPack && expected == 0 && count >= 5 {
				seasonsComplete[stream.Season] = true
			}
		}
	}

	// Process singles
	singlesProcessed := 0
	for _, stream := range streams {
		if stream.IsFullpack {
			continue
		}
		if singlesProcessed >= tvSinglesLimit {
			break
		}
		if skippedSeasons[stream.Season] {
			continue
		}
		if seasonsComplete[stream.Season] {
			continue
		}

		count := e.processSingle(ctx, showName, imdbID, e.showDir(show), stream, show.FirstAirDate, knownTitles)
		created += count
		singlesProcessed++
	}

	if created > 0 {
		e.stats.Shows++
		e.stats.EpisodesCreated += created
	}
	return searched
}

type TVStream struct {
	Title         string
	Hash          string
	IsFullpack    bool
	IsPartialPack bool
	QualityScore  int
	Season        int
	EpisodeNum    int
	Seeders       int
	SizeGB        float64
	Priority      int
}

// seasonWindow returns how many trailing seasons to consider. A configured
// value of 0 or less means the whole show, expressed as numSeasons so the
// caller's startSeason arithmetic still lands on 1.
func (e *TVGoEngine) seasonWindow(numSeasons int) int {
	if e.maxSeasons <= 0 {
		return numSeasons
	}
	return e.maxSeasons
}

// seasonSearch is what one season's search established. Complete means every source
// that should have answered did; RawSeen means releases naming that season came back
// at all, before any quality gate. They are kept apart because they answer different
// questions: whether the search ran, and whether the season exists in the catalogues.
//
// RawSeen is deliberately pre-gate on both sources. A release rejected for seeders or
// language still proves the season is out there, and measuring it after the gates
// would make the same reality read differently depending on which indexer answered.
type seasonSearch struct {
	Complete bool
	RawSeen  bool
}

// getStreams returns the classified streams and, per season, what its search
// established. Completeness is per season because the two sources cover different
// ones: Prowlarr answers for the window, Torrentio fills only the seasons it left
// uncovered. A single show-level flag would call a season searched when nothing ever
// asked for it.
//
// seasons, when non-empty, replaces the default window (seasonWindow): the reaper
// needs the seasons a dead release actually covers, which for an old pack are
// nowhere near the end of the show.
func (e *TVGoEngine) getStreams(ctx context.Context, imdbID string, tmdbID int, showName string, details *tmdb.TVDetail, seasons ...int) ([]TVStream, map[int]seasonSearch) {
	numSeasons := details.NumberOfSeasons
	if numSeasons == 0 {
		numSeasons = 5
	}

	maxSeasons := e.seasonWindow(numSeasons)
	startSeason := numSeasons - maxSeasons + 1
	if startSeason < 1 {
		startSeason = 1
	}
	endSeason := numSeasons
	if len(seasons) > 0 {
		startSeason, endSeason = seasons[0], seasons[0]
		for _, s := range seasons {
			if s < startSeason {
				startSeason = s
			}
			if s > endSeason {
				endSeason = s
			}
		}
	}

	var allStreams []prowlarr.Stream
	seenHashes := make(map[string]bool)

	// Build TMDB episode cap per season — used to reject streams beyond the canonical count
	tmdbSeasonEps := make(map[int]int)
	for _, sd := range details.Seasons {
		if sd.SeasonNumber > 0 && sd.EpisodeCount > 0 {
			tmdbSeasonEps[sd.SeasonNumber] = sd.EpisodeCount
		}
	}

	// An episode that has not aired cannot have a release, so the cap for the season
	// still airing is what has aired, not what TMDB lists. Without it the fallback asks
	// Torrentio for future episodes and gets releases of a same-named older show back
	// (production: "Dark Matter S02E10 Take the Shot" is Dark Matter 2015, not 2024).
	airedSeasonEps := make(map[int]int, len(tmdbSeasonEps))
	for season, count := range tmdbSeasonEps {
		airedSeasonEps[season] = count
	}
	if next := details.NextEpisodeToAir; next != nil && next.EpisodeNumber > 0 {
		airedSeasonEps[next.SeasonNumber] = next.EpisodeNumber - 1
	}

	// Prowlarr primary
	prowlarrComplete := true
	if e.prowlarr != nil {
		tp := time.Now()
		var targetSeasons []int
		for s := startSeason; s <= endSeason; s++ {
			targetSeasons = append(targetSeasons, s)
		}
		streams, complete, err := e.prowlarr.FetchTorrentsStatus(imdbID, "series", showName, 0, nil, targetSeasons...)
		if err != nil {
			e.logger.Printf("Prowlarr search failed for %s: %v", showName, err)
			streams = nil
			prowlarrComplete = false
		} else if !complete {
			e.logger.Printf("Prowlarr answered partially for %s: seasons are not conclusively searched", showName)
			prowlarrComplete = false
		}
		for _, s := range streams {
			h := strings.ToLower(s.InfoHash)
			if h != "" && !seenHashes[h] {
				seenHashes[h] = true
				allStreams = append(allStreams, s)
			}
		}
		e.logger.Printf("    Prowlarr: %d streams in %v", len(allStreams), time.Since(tp).Round(time.Millisecond))
	}

	// Which target seasons did Prowlarr actually cover? Coverage is per season, not
	// per show: an indexer that ignores the season qualifier answers a "Show s04" query
	// with S03 releases, which are legitimate for the window and would otherwise pass as
	// "Prowlarr worked" and suppress the fallback for the season that has no results at
	// all (production: Dark Matter S02, Ted Lasso S04).
	covered := make(map[int]bool)
	for _, c := range e.classifyAndFilter(allStreams, startSeason, endSeason, airedSeasonEps) {
		covered[c.Season] = true
	}
	var missing []int
	for s := startSeason; s <= endSeason; s++ {
		if !covered[s] {
			missing = append(missing, s)
		}
	}

	// Torrentio fallback, restricted to the uncovered seasons and merged with Prowlarr's
	// results rather than replacing them, so ITA releases already found are kept.
	// A season Prowlarr covered is as complete as Prowlarr's own answer; one it left
	// to the fallback is complete only if every aired episode was actually fetched.
	// Raw coverage, before the quality gates: a release that names the season counts
	// even if classifyStream would discard it.
	rawSeen := make(map[int]bool)
	for _, st := range allStreams {
		if span := e.extractSeasonSpan(st.Title); span != nil {
			for season := span[0]; season <= span[1]; season++ {
				rawSeen[season] = true
			}
			continue
		}
		// extractSeason falls back to 1 when the title names no season, so a release
		// that mentions none is attributed there rather than to a phantom season 0.
		if season := e.extractSeason(st.Title); season > 0 {
			rawSeen[season] = true
		}
	}

	seasonComplete := make(map[int]seasonSearch, endSeason-startSeason+1)
	for s := startSeason; s <= endSeason; s++ {
		seasonComplete[s] = seasonSearch{Complete: prowlarrComplete && covered[s], RawSeen: rawSeen[s]}
	}

	if len(missing) > 0 {
		tt := time.Now()
		epsFetched := 0
		for _, season := range missing {
			epCount := airedSeasonEps[season]
			if epCount == 0 {
				continue // no aired episode for this season — nothing legitimate to fetch
			}
			seasonFetchFailed := false
			for ep := 1; ep <= epCount; ep++ {
				tioStreams, err := e.torrentio.FetchEpisodeStreams(ctx, imdbID, season, ep)
				if err != nil {
					seasonFetchFailed = true
					continue
				}
				epsFetched++
				if len(tioStreams) > 0 {
					// Same meaning as the Prowlarr side: releases came back for this
					// season, whatever the gates will make of them.
					rawSeen[season] = true
				}
				for _, s := range tioStreams {
					// Prowlarr's hashes are stored lowercase; normalize so the merge dedups.
					h := strings.ToLower(s.InfoHash)
					if h != "" && !seenHashes[h] {
						seenHashes[h] = true
						allStreams = append(allStreams, prowlarr.Stream{
							Name:     s.Name,
							Title:    s.Title,
							InfoHash: s.InfoHash,
						})
					}
				}
				e.limiter.Wait(ctx)
			}
			// RawSeen stays apart from Complete: every episode answering 200 with an
			// empty list is a healthy search of a season with nothing to offer, and is
			// indistinguishable here from a degraded indexer. The caller decides what
			// to do with that, this only reports it.
			seasonComplete[season] = seasonSearch{
				Complete: prowlarrComplete && !seasonFetchFailed,
				RawSeen:  rawSeen[season],
			}
		}
		e.logger.Printf("    Torrentio fallback for S%v: %d streams total from %d eps in %v", missing, len(allStreams), epsFetched, time.Since(tt).Round(time.Millisecond))
	}

	return e.classifyAndFilter(allStreams, startSeason, endSeason, airedSeasonEps), seasonComplete
}

// classifyAndFilter runs classifyStream on each raw stream and keeps only those matching the
// target season range and TMDB's canonical episode count. Used both to decide whether Prowlarr's
// results are usable at all (fallback trigger) and to build the final result, so both checks stay
// in sync.
func (e *TVGoEngine) classifyAndFilter(streams []prowlarr.Stream, startSeason, endSeason int, tmdbSeasonEps map[int]int) []TVStream {
	var classified []TVStream
	for _, s := range streams {
		c := e.classifyStream(s)
		if c == nil {
			continue
		}
		if c.Season < startSeason || c.Season > endSeason {
			continue
		}
		// Reject single-episode streams beyond TMDB's canonical episode count
		if !c.IsFullpack && c.EpisodeNum > 0 {
			if maxEp, ok := tmdbSeasonEps[c.Season]; ok && c.EpisodeNum > maxEp {
				continue
			}
		}
		span := e.extractSeasonSpan(c.Title)
		if span != nil && span[0] < startSeason {
			continue
		}
		classified = append(classified, *c)
	}
	return classified
}

func (e *TVGoEngine) classifyStream(s prowlarr.Stream) *TVStream {
	title := s.Title
	name := s.Name
	fullText := title + " " + name

	// Hash blacklist check
	if e.isHashBlacklisted(s.InfoHash) {
		return nil
	}

	// A release this run already found silent: re-selecting it would cost a full
	// metadata wait to learn what we know.
	if e.isDeadPackHash(s.InfoHash) {
		return nil
	}

	// Title blacklist check
	if e.isBlacklisted(title) {
		return nil
	}

	// A fan re-edit numbers its files its own way: One Pace's batch was a
	// candidate for ONE PIECE's first season.
	if reTVFanEdit.MatchString(title) {
		return nil
	}

	seeders := e.extractSeeders(title)
	qualityScore := e.calculateQualityScore(fullText, seeders, e.weights)

	if qualityScore == 0 {
		return nil
	}

	is4K := reTV4K.MatchString(fullText)
	minReq := e.weights.MinSeeders4K
	if !is4K {
		minReq = e.weights.MinSeeders
	}
	if seeders < minReq {
		return nil
	}

	if e.reExclLang.MatchString(title) {
		return nil
	}

	isFullpack := e.isFullpack(title)
	season := e.extractSeason(title)
	episodeNum := 0
	if !isFullpack {
		if m := reTVEpNum.FindStringSubmatch(title); m != nil {
			episodeNum, _ = strconv.Atoi(m[2])
		}
	}
	isPartialPack := false
	if isFullpack {
		isPartialPack = reTVRange.MatchString(strings.Split(title, "\n")[0])
	}

	priorityBonus := 0
	if isFullpack {
		if isPartialPack {
			priorityBonus = e.weights.Fullpack / 2
		} else {
			priorityBonus = e.weights.Fullpack
		}
	}

	return &TVStream{
		Title:         title,
		Hash:          strings.ToLower(s.InfoHash),
		IsFullpack:    isFullpack,
		IsPartialPack: isPartialPack,
		QualityScore:  qualityScore,
		Season:        season,
		EpisodeNum:    episodeNum,
		Seeders:       seeders,
		SizeGB:        s.SizeGB,
		Priority:      qualityScore + priorityBonus,
	}
}

func (e *TVGoEngine) calculateQualityScore(text string, seeders int, w config.TVWeights) int {
	t := strings.ToLower(text)
	score := 0

	if reTV4K.MatchString(t) {
		score += w.Res4K
	} else if reTV1080p.MatchString(t) {
		score += w.Res1080p
	} else {
		return 0
	}

	if reTVDV.MatchString(t) {
		score += w.DolbyVision
	} else if reTVHDR.MatchString(t) {
		score += w.HDR
	}

	if reTVAtmos.MatchString(t) {
		score += w.Atmos
	} else if reTV51.MatchString(t) {
		score += w.Audio51
	}

	if e.reITA.MatchString(t) {
		score += w.PreferredLanguage
	}

	if seeders >= 100 {
		score += w.SeederTier100
	} else if seeders >= 50 {
		score += w.SeederTier50
	} else if seeders >= 20 {
		score += w.SeederTier20
	}

	return score
}

func (e *TVGoEngine) isFullpack(title string) bool {
	firstLine := strings.Split(title, "\n")[0]
	t := strings.ToLower(firstLine)

	if reTVFullpack.MatchString(t) {
		return true
	}
	if reTVRange.MatchString(t) {
		return true
	}
	if len(reTVMultiEp.FindAllString(t, -1)) >= 2 {
		return true
	}
	if reTVSeason.MatchString(t) && !reTVMultiEp.MatchString(t) {
		// Exclude single specials: "Show.S02.Christmas.Special" → not a fullpack
		if !reTVSpecialTitle.MatchString(t) {
			return true
		}
	}
	if reTVSeasonP.MatchString(t) && !reTVMultiEp.MatchString(t) {
		if !reTVSpecialTitle.MatchString(t) {
			return true
		}
	}
	return false
}

func (e *TVGoEngine) extractSeason(title string) int {
	m := reTVSeasonN.FindStringSubmatch(title)
	if len(m) > 1 {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 1
}

func (e *TVGoEngine) extractSeasonSpan(title string) *[2]int {
	firstLine := strings.ToLower(strings.Split(title, "\n")[0])

	m := reTVSeasonR.FindStringSubmatch(firstLine)
	if len(m) > 2 {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		if a > b {
			a, b = b, a
		}
		return &[2]int{a, b}
	}

	m = reTVSeasonW.FindStringSubmatch(firstLine)
	if len(m) > 2 {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		if a > b {
			a, b = b, a
		}
		return &[2]int{a, b}
	}

	if reTVCompleteS.MatchString(firstLine) {
		return &[2]int{1, 99}
	}

	return nil
}

func (e *TVGoEngine) extractSeeders(title string) int {
	m := reTVSeeders.FindStringSubmatch(title)
	if len(m) > 1 {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// targetDir is the tree this show's stubs go in: tvDir, or animeDir when the
// show is anime. It is passed rather than read from the engine because the
// choice is per show, and processShow is the only place that knows which.
func (e *TVGoEngine) processFullpack(ctx context.Context, showName, showIMDB, targetDir string, stream TVStream, firstAirDate string, knownTitles []string) int {
	magnet := BuildMagnet(stream.Hash, stream.Title, DefaultTrackers())
	hash, err := e.gostorm.AddTorrent(ctx, magnet, stream.Title)
	if err != nil || hash == "" {
		return 0
	}

	info, err := e.gostorm.GetTorrentInfo(ctx, hash, 90)
	if err != nil {
		e.gostorm.RemoveTorrent(ctx, hash)
		return 0
	}

	var videoFiles []FileStat
	for _, f := range info.FileStats {
		if e.isVideoFile(f.Path) {
			if f.Length >= tvMinEpisodeSize && f.Length <= tvMaxEpisodeSize {
				videoFiles = append(videoFiles, f)
			}
		}
	}

	if len(videoFiles) == 0 {
		e.gostorm.RemoveTorrent(ctx, hash)
		return 0
	}

	if !e.contentsBelongToShow(videoFiles, knownTitles, showName, stream.Title) {
		e.gostorm.RemoveTorrent(ctx, hash)
		return 0
	}

	created := 0
	skipped := 0
	cleanShow := e.getShowFolderName(showName, firstAirDate)

	for _, vf := range videoFiles {
		filename := filepath.Base(vf.Path)
		epInfo := e.extractEpisodeFromFilename(filename)
		if epInfo[0] == 0 && epInfo[1] == 0 {
			continue
		}

		season, episode := epInfo[0], epInfo[1]
		key := e.episodeKey(showName, season, episode)

		if e.processedThisRun[key] {
			continue
		}

		if existing, ok := e.registry[key]; ok && !e.isDeadEpisode(key) {
			if float64(stream.QualityScore) <= float64(existing.QualityScore)*tvUpgradeThreshold {
				e.stats.EpisodesSkipped++
				skipped++
				e.processedThisRun[key] = true
				continue
			}
		}

		seasonDir := filepath.Join(targetDir, cleanShow, fmt.Sprintf("Season.%02d", season))
		epFilename := e.buildFilename(showName, season, episode, hash[:8])
		epPath := filepath.Join(seasonDir, epFilename)
		streamURL := fmt.Sprintf("%s/stream?link=%s&index=%d&play", e.gostorm.baseURL, hash, vf.ID)

		if e.createMKV(epPath, streamURL, vf.Length, magnet) {
			if existing, ok := e.registry[key]; ok && existing.FilePath != "" && existing.FilePath != epPath {
				e.removeStub(ctx, existing.FilePath, existing.Hash)
				e.stats.Upgrades++
			}
			e.registerEpisode(key, stream.QualityScore, hash, epPath, "fullpack", showIMDB)
			e.processedThisRun[key] = true
			created++
			e.logger.Printf("Created: %s", epFilename)
		}
	}

	if skipped > 0 && created == 0 {
		e.logger.Printf("  fullpack skipped: %d/%d eps already at sufficient quality (score %d)", skipped, len(videoFiles), stream.QualityScore)
	}
	if created == 0 {
		e.gostorm.RemoveTorrent(ctx, hash)
	}

	return created
}

func (e *TVGoEngine) processSingle(ctx context.Context, showName, showIMDB, targetDir string, stream TVStream, firstAirDate string, knownTitles []string) int {
	title := stream.Title
	m := reTVEpNum.FindStringSubmatch(title)
	if len(m) < 3 {
		return 0
	}
	episode, _ := strconv.Atoi(m[2])
	season := stream.Season

	key := e.episodeKey(showName, season, episode)

	if e.processedThisRun[key] {
		return 0
	}

	if existing, ok := e.registry[key]; ok && !e.isDeadEpisode(key) {
		if float64(stream.QualityScore) <= float64(existing.QualityScore)*tvUpgradeThreshold {
			e.stats.EpisodesSkipped++
			e.processedThisRun[key] = true
			e.logger.Printf("  Skip S%02dE%02d: score %d <= existing %d (threshold %.0f)", season, episode, stream.QualityScore, existing.QualityScore, float64(existing.QualityScore)*tvUpgradeThreshold)
			return 0
		}
	}

	magnet := BuildMagnet(stream.Hash, title, DefaultTrackers())
	hash, err := e.gostorm.AddTorrent(ctx, magnet, title)
	if err != nil || hash == "" {
		return 0
	}

	info, err := e.gostorm.GetTorrentInfo(ctx, hash, 45)
	if err != nil {
		e.gostorm.RemoveTorrent(ctx, hash)
		return 0
	}

	var bestFile *FileStat
	for i := range info.FileStats {
		f := &info.FileStats[i]
		if e.isVideoFile(f.Path) && f.Length >= tvMinEpisodeSize {
			if bestFile == nil || f.Length > bestFile.Length {
				cp := *f
				bestFile = &cp
			}
		}
	}
	if bestFile == nil {
		e.gostorm.RemoveTorrent(ctx, hash)
		return 0
	}

	if !e.contentsBelongToShow([]FileStat{*bestFile}, knownTitles, showName, title) {
		e.gostorm.RemoveTorrent(ctx, hash)
		return 0
	}

	cleanShow := e.getShowFolderName(showName, firstAirDate)
	seasonDir := filepath.Join(targetDir, cleanShow, fmt.Sprintf("Season.%02d", season))
	epFilename := e.buildFilename(showName, season, episode, hash[:8])
	epPath := filepath.Join(seasonDir, epFilename)
	streamURL := fmt.Sprintf("%s/stream?link=%s&index=%d&play", e.gostorm.baseURL, hash, bestFile.ID)

	if e.createMKV(epPath, streamURL, bestFile.Length, magnet) {
		if existing, ok := e.registry[key]; ok && existing.FilePath != "" && existing.FilePath != epPath {
			e.removeStub(ctx, existing.FilePath, existing.Hash)
			e.stats.Upgrades++
		}
		e.registerEpisode(key, stream.QualityScore, hash, epPath, "single", showIMDB)
		e.processedThisRun[key] = true
		e.logger.Printf("Created: %s", epFilename)
		return 1
	}

	return 0
}

func (e *TVGoEngine) getCompleteSeasons(showName string, details *tmdb.TVDetail) map[int]float64 {
	normalized := reTVNonWord.ReplaceAllString(strings.ToLower(showName), "")

	seasonEps := make(map[int]int)
	for _, sd := range details.Seasons {
		if sd.SeasonNumber > 0 && sd.EpisodeCount > 0 {
			seasonEps[sd.SeasonNumber] = sd.EpisodeCount
		}
	}

	complete := make(map[int]float64)
	for sn, expected := range seasonEps {
		var scores []int
		// The key is "<normalized show>_sXXeYY", so the show segment has to be anchored:
		// a bare prefix counts every show whose name starts the same way, and "ted"
		// would be filled in by "tedlasso" (nine such pairs in production).
		prefix := fmt.Sprintf("%s_s%02de", normalized, sn)
		for key, entry := range e.registry {
			// A dead episode does not count towards its season: leaving it in would keep
			// the season "complete", and the cascade below would skip the very search
			// meant to replace it.
			if strings.HasPrefix(key, prefix) && !e.isDeadEpisode(key) {
				scores = append(scores, entry.QualityScore)
			}
		}
		if len(scores) >= expected {
			sum := 0
			for _, s := range scores {
				sum += s
			}
			complete[sn] = float64(sum) / float64(len(scores))
		}
	}

	return complete
}

// reconcileRegistry removes registry entries whose backing MKV file no longer
// exists on disk (ghost entries). It runs before the main show-processing loop
// so that the current sync can immediately search for and recreate missing episodes.
func (e *TVGoEngine) reconcileRegistry() {
	removed := 0
	for key, entry := range e.registry {
		if _, err := os.Stat(entry.FilePath); os.IsNotExist(err) {
			delete(e.registry, key)
			if e.db != nil {
				if err := e.db.DeleteEpisode(key); err != nil {
					e.logger.Printf("[TVSync] Warning: failed to delete ghost entry %s from DB: %v", key, err)
				}
			}
			removed++
			e.logger.Printf("[TVSync] Ghost entry removed: %s (missing: %s)", key, entry.FilePath)
		}
	}
	if removed > 0 {
		e.logger.Printf("[TVSync] Reconciliation complete: %d ghost entries removed", removed)
	}
}

func (e *TVGoEngine) cleanupOrphanedFiles(ctx context.Context) {
	regPaths := make(map[string]bool)
	for _, entry := range e.registry {
		regPaths[entry.FilePath] = true
	}

	e.walkRoots(func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		if !regPaths[path] {
			e.removeStub(ctx, path, e.readHashFromMKV(path))
		}
		return nil
	})

	// Remove empty season and show directories (deepest first)
	var dirs []string
	e.walkRoots(func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && !e.isRoot(path) {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		entries, _ := os.ReadDir(dirs[i])
		if len(entries) == 0 {
			e.removeStub(ctx, dirs[i], "")
		}
	}
}

func (e *TVGoEngine) rehydrateMissingTorrents(ctx context.Context) {
	torrents, err := e.gostorm.ListTorrents(ctx)
	if err != nil {
		return
	}
	activeHashes := make(map[string]bool)
	for _, t := range torrents {
		activeHashes[t.Hash] = true
	}

	rehydrated := 0
	e.logger.Printf("Scanning for missing torrents to rehydrate...")

	e.walkRoots(func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var url, magnet string
		var size float64
		content := strings.TrimSpace(string(data))

		if strings.HasPrefix(content, "{") {
			var obj map[string]interface{}
			if err := json.Unmarshal(data, &obj); err != nil {
				return nil
			}
			url, _ = obj["url"].(string)
			magnet, _ = obj["magnet"].(string)
			size, _ = obj["size"].(float64)
		} else {
			lines := strings.SplitN(content, "\n", 4)
			if len(lines) < 3 {
				return nil
			}
			url = strings.TrimSpace(lines[0])
			magnet = strings.TrimSpace(lines[2])
			if len(lines) > 1 {
				size, _ = strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
			}
		}

		m := reTVHashURL.FindStringSubmatch(url)
		if len(m) < 2 {
			return nil
		}
		hash := strings.ToLower(m[1])

		if activeHashes[hash] {
			return nil
		}

		if strings.HasPrefix(magnet, "magnet:?") {
			displayTitle := TitleFromFilename(info.Name())
			freshMagnet := BuildMagnet(hash, displayTitle, DefaultTrackers())
			e.logger.Printf("Rehydrating #%d: %s...", rehydrated+1, info.Name())
			if _, err := e.gostorm.AddTorrent(ctx, freshMagnet, displayTitle); err == nil {
				e.createMKV(path, url, int64(size), freshMagnet)
				rehydrated++
				activeHashes[hash] = true
				time.Sleep(5 * time.Second)
			}
		}

		return nil
	})

	if rehydrated > 0 {
		e.logger.Printf("Rehydrated %d missing torrents", rehydrated)
	} else {
		e.logger.Printf("No torrents needed rehydration")
	}
}

func (e *TVGoEngine) cleanupOrphanedTorrents(ctx context.Context) {
	torrents, err := e.gostorm.ListTorrents(ctx)
	if err != nil {
		return
	}

	registryHashes := make(map[string]bool)
	for _, entry := range e.registry {
		registryHashes[strings.ToLower(entry.Hash)] = true
	}

	// Also collect hashes from disk files
	diskHashes := make(map[string]bool)
	e.walkRoots(func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := strings.TrimSpace(string(data))
		if strings.HasPrefix(content, "{") {
			var obj map[string]interface{}
			if err := json.Unmarshal(data, &obj); err != nil {
				return nil
			}
			url, _ := obj["url"].(string)
			m := reTVHashURL.FindStringSubmatch(url)
			if len(m) >= 2 {
				diskHashes[strings.ToLower(m[1])] = true
			}
		} else {
			lines := strings.SplitN(content, "\n", 2)
			if len(lines) >= 1 {
				m := reTVHashURL.FindStringSubmatch(lines[0])
				if len(m) >= 2 {
					diskHashes[strings.ToLower(m[1])] = true
				}
			}
		}
		return nil
	})

	reTVSeries := regexp.MustCompile(`(?i)s\d+e\d+|season|episode`)
	removed := 0

	for _, t := range torrents {
		h := strings.ToLower(t.Hash)
		if h == "" {
			continue
		}
		if !reTVSeries.MatchString(t.Title) {
			continue
		}
		if registryHashes[h] || diskHashes[h] {
			continue
		}
		if e.gostorm.RemoveTorrent(ctx, h) == nil {
			removed++
			e.logger.Printf("Removed orphaned torrent: %s...", h[:8])
		}
	}

	if removed > 0 {
		e.logger.Printf("Removed %d orphaned torrents", removed)
	}
}

func (e *TVGoEngine) isVideoFile(path string) bool {
	return library.IsVideoFile(path)
}

func (e *TVGoEngine) extractEpisodeFromFilename(filename string) [2]int {
	season, episode := library.ParseSeasonEpisode(filename)
	return [2]int{season, episode}
}

func (e *TVGoEngine) sanitizeName(name string) string {
	return library.SanitizeShowName(name)
}

func (e *TVGoEngine) getShowFolderName(showName, firstAirDate string) string {
	return library.ShowFolderName(showName, firstAirDate)
}

func (e *TVGoEngine) buildFilename(show string, season, episode int, hash8 string) string {
	return library.EpisodeFilename(show, season, episode, hash8)
}

// createMKV writes an episode stub. The imdb field stays empty on purpose: the
// webhook matcher pairs a Plex episode event with an open file by looking at states
// whose id is empty, and a series id here would make every open episode of the same
// show match the same event. The show id lives in tv_episodes.show_imdb instead,
// which is where anything that needs to trace a file back to TMDB should read it.
func (e *TVGoEngine) createMKV(path, streamURL string, fileSize int64, magnet string) bool {
	return library.WriteStub(path, streamURL, fileSize, magnet, "") == nil
}

func (e *TVGoEngine) showKnownTitles(ctx context.Context, tmdbID int, details *tmdb.TVDetail) []string {
	if t, ok := e.knownTitles[tmdbID]; ok {
		return t
	}
	titles := []string{}
	if details != nil {
		titles = append(titles, details.Name, details.OriginalName)
	}
	if alts, err := e.tmdb.TVAlternativeTitles(ctx, tmdbID); err == nil {
		titles = append(titles, alts...)
	} else {
		e.logger.Printf("    [ShowMatch] alternative titles unavailable for tmdb=%d: %v", tmdbID, err)
	}
	// alternative_titles is incomplete — it carries no Italian entry for Lioness, whose
	// releases are all named "Operazione Speciale Lioness". The localized show name does.
	titles = append(titles, e.tmdb.TVLocalizedNames(ctx, tmdbID, e.tmdbLangs)...)
	e.knownTitles[tmdbID] = titles
	return titles
}

// contentsBelongToShow checks a torrent's real file names against the show we asked
// for. A mismatch is always logged as [ShowMatch]; tvShowMatchEnforce decides
// whether it is also discarded.
func (e *TVGoEngine) contentsBelongToShow(files []FileStat, knownTitles []string, showName, releaseTitle string) bool {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Path)
	}
	matched, decidable := filesMatchShow(names, knownTitles)
	if matched || !decidable {
		return true
	}
	e.logger.Printf("    [ShowMatch] %s: contents do not match (%q) — %s",
		showName, showPrefixFromFilename(names[0]), releaseTitle[:min(70, len(releaseTitle))])
	return !tvShowMatchEnforce
}
