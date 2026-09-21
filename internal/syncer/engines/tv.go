package engines

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/config"
	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
	"tiramisu/internal/prowlarr"
)

// TVSyncMode chooses which series a TV engine run covers.
type TVSyncMode string

const (
	// TVModeAll covers every series, anime included, as upstream does.
	TVModeAll TVSyncMode = ""
	// TVModeTV covers the series outside the anime tree.
	TVModeTV TVSyncMode = "tv"
	// TVModeAnime covers anime alone.
	TVModeAnime TVSyncMode = "anime"
)

// TVSyncer runs the TV sync in pure Go (Fase 3).
type TVSyncer struct {
	engine *TVGoEngine
	name   string
}

// TVSyncerConfig holds config for the Go TV engine.
type TVSyncerConfig struct {
	// Name is the scheduler job name, and names the log file: "tv" by default,
	// "anime" for the anime job.
	Name string
	// Mode is read at the start of every run, so enabling the anime job in the
	// control panel takes anime out of the TV job without a restart. Nil means
	// TVModeAll.
	Mode            func() TVSyncMode
	GoStormURL      string
	TMDBAPIKey      string
	TorrentioURL    string
	PlexURL         string
	PlexToken       string
	MediaServerType string
	PlexTVLib       int
	TVDir           string
	AnimeDir        string
	AnimeEnabled    bool
	AnimeGenreIDs   []int
	AnimeLanguages  []string
	StateDir        string
	LogsDir         string
	ProwlarrCfg     prowlarr.ConfigProwlarr
	Language        config.LanguageConfig
	QualityScoring  config.QualityScoringConfig
	MaxSeasons      int
	DB              *metadb.DB // V1.7.1: Optional SQLite backend
	AudioRegistry   library.AudioRegistry
	// InvalidatePath, when set, is called after removing a stub file/dir so the FUSE
	// layer drops its cached state for it (see main.invalidateSyncRemovedPath).
	InvalidatePath func(string)
}

// NewTVSyncer creates a new Go-based TV syncer.
func NewTVSyncer(cfg TVSyncerConfig) *TVSyncer {
	exe, _ := os.Executable()
	binDir := filepath.Dir(exe)

	tvDir := cfg.TVDir
	if tvDir == "" {
		tvDir = "/mnt/torrserver/tv"
	}
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(binDir, "STATE")
	}
	logsDir := cfg.LogsDir
	if logsDir == "" {
		logsDir = filepath.Join(binDir, "logs")
	}
	name := cfg.Name
	if name == "" {
		name = "tv"
	}

	engineCfg := TVEngineConfig{
		Name:            name,
		Mode:            cfg.Mode,
		GoStormURL:      cfg.GoStormURL,
		TMDBAPIKey:      cfg.TMDBAPIKey,
		TorrentioURL:    cfg.TorrentioURL,
		PlexURL:         cfg.PlexURL,
		PlexToken:       cfg.PlexToken,
		MediaServerType: cfg.MediaServerType,
		PlexTVLib:       cfg.PlexTVLib,
		TVDir:           tvDir,
		AnimeDir:        cfg.AnimeDir,
		AnimeEnabled:    cfg.AnimeEnabled,
		AnimeGenreIDs:   cfg.AnimeGenreIDs,
		AnimeLanguages:  cfg.AnimeLanguages,
		StateDir:        stateDir,
		LogsDir:         logsDir,
		ProwlarrCfg:     cfg.ProwlarrCfg,
		Language:        cfg.Language,
		Weights:         cfg.QualityScoring.TVWeights(),
		MaxSeasons:      cfg.MaxSeasons,
		InvalidatePath:  cfg.InvalidatePath,
		AudioRegistry:   cfg.AudioRegistry,
	}

	engine := NewTVGoEngine(engineCfg, cfg.DB)
	engine.name = name
	engine.mode = cfg.Mode
	return &TVSyncer{engine: engine, name: name}
}

func (s *TVSyncer) Name() string { return s.name }

func (s *TVSyncer) Run(ctx context.Context) error {
	if err := s.engine.Run(ctx); err != nil {
		return fmt.Errorf("%s sync: %w", s.name, err)
	}
	return nil
}

// apiEpisodeScore is the quality an episode filed through the Library API counts
// as. Those episodes are deliberate: a Seerr request, the gap fill, or a re-file
// that checked each file's name and translated its numbering. The sync asks
// Torrentio for TMDB's episode numbers, and where those differ from Cinemeta's
// (ONE PIECE, anime split into cours) its "upgrade" is another episode. Scored
// this high, they are never replaced, and a season they fill counts as complete,
// so it is not queried again on every run.
const apiEpisodeScore = 1 << 30

// protectedScore is the score a registry row is compared at. Only the copy the
// engine holds in memory is raised; the database keeps the real score.
func protectedScore(score int, source string) int {
	if source == "api" && score < apiEpisodeScore {
		return apiEpisodeScore
	}
	return score
}

// reTVFanEdit matches fan re-edits, which cut and renumber episodes their own
// way: "One Pace", "Chronologically LOST".
var reTVFanEdit = regexp.MustCompile(`(?i)(^|[^a-z])(one[ ._-]?pace|chronologically|fan[ ._-]?edit|recut)([^a-z]|$)`)

// currentMode is the series this engine's runs cover now.
func (e *TVGoEngine) currentMode() TVSyncMode {
	if e.mode == nil {
		return TVModeAll
	}
	return e.mode()
}

// wants reports whether a discovered show belongs to the current mode.
func (e *TVGoEngine) wants(show tmdb.TVShow) bool {
	switch e.currentMode() {
	case TVModeTV:
		return !e.isAnimeShow(show)
	case TVModeAnime:
		return e.isAnimeShow(show)
	}
	return true
}
