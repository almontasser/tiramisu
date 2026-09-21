package metadb

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var reKeyNonWord = regexp.MustCompile(`[^a-z0-9]`)

// EpisodeEntry represents a TV episode registry entry.
type EpisodeEntry struct {
	EpisodeKey   string
	QualityScore int
	Hash         string
	FilePath     string
	Source       string
	Created      int64 // unix timestamp
	// ShowIMDB is the show's id, not the episode's: it is what lets a stub be traced
	// back to TMDB when the discovery feed no longer returns the show.
	ShowIMDB string
}

// UpsertEpisode inserts or updates an episode entry.
func (d *DB) UpsertEpisode(key string, entry EpisodeEntry) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO tv_episodes
		 (episode_key, quality_score, hash, file_path, source, created, show_imdb)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		key, entry.QualityScore, entry.Hash, entry.FilePath, entry.Source, entry.Created, entry.ShowIMDB,
	)
	return err
}

// SetEpisodeShowIMDB fills in the show id of one episode, and only that.
// Deliberately not an UpsertEpisode with a modified entry: that is an INSERT OR
// REPLACE, so a caller holding a row read earlier in the run would write back its
// stale hash and path over whatever replaced them since — and resurrect a row the
// meantime deleted. An UPDATE touches the one column it means to, and touches
// nothing at all when the episode is gone.
func (d *DB) SetEpisodeShowIMDB(key, imdbID string) error {
	_, err := d.db.Exec(
		`UPDATE tv_episodes SET show_imdb = ?, updated_at = datetime('now') WHERE episode_key = ?`,
		imdbID, key,
	)
	return err
}

// GetEpisode returns a single episode by its key.
func (d *DB) GetEpisode(key string) (*EpisodeEntry, bool, error) {
	var e EpisodeEntry
	err := d.db.QueryRow(
		"SELECT episode_key, quality_score, hash, file_path, source, created, COALESCE(show_imdb, '') FROM tv_episodes WHERE episode_key = ?",
		key,
	).Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created, &e.ShowIMDB)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &e, true, nil
}

// DeleteEpisode deletes an episode by its key.
func (d *DB) DeleteEpisode(key string) error {
	_, err := d.db.Exec("DELETE FROM tv_episodes WHERE episode_key = ?", key)
	return err
}

// AllEpisodes returns all episode entries.
func (d *DB) AllEpisodes() ([]EpisodeEntry, error) {
	rows, err := d.db.Query(
		"SELECT episode_key, quality_score, hash, file_path, source, created, COALESCE(show_imdb, '') FROM tv_episodes",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []EpisodeEntry
	for rows.Next() {
		var e EpisodeEntry
		if err := rows.Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created, &e.ShowIMDB); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// EpisodesByFilePath returns all episodes that reference a given file path.
// Used for cleanup by file existence.
func (d *DB) EpisodesByFilePath(filePath string) ([]EpisodeEntry, error) {
	rows, err := d.db.Query(
		"SELECT episode_key, quality_score, hash, file_path, source, created, COALESCE(show_imdb, '') FROM tv_episodes WHERE file_path = ?",
		filePath,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []EpisodeEntry
	for rows.Next() {
		var e EpisodeEntry
		if err := rows.Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created, &e.ShowIMDB); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// EpisodesByHash returns every episode backed by one torrent. A season pack stands
// behind many episodes, so a dead hash is never a single-file decision; idx_tv_hash
// makes this the cheap way to ask.
func (d *DB) EpisodesByHash(hash string) ([]EpisodeEntry, error) {
	rows, err := d.db.Query(
		`SELECT episode_key, quality_score, hash, file_path, source, created, COALESCE(show_imdb, '')
		 FROM tv_episodes WHERE hash = ? ORDER BY episode_key`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpisodeEntry
	for rows.Next() {
		var e EpisodeEntry
		if err := rows.Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created, &e.ShowIMDB); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// folderEpisodeKey rebuilds a registry key the way library.EpisodeKey builds it now,
// from the show folder, which carries the year: ".../ONE_PIECE (1999)/Season.01/x.mkv"
// turns "onepiece_s01e01" into "onepiece1999_s01e01". A path that is not
// <show>/Season.NN/<file> keeps its key.
func folderEpisodeKey(key, path string) string {
	i := strings.LastIndex(key, "_s")
	season := filepath.Dir(path)
	if i < 0 || !strings.HasPrefix(filepath.Base(season), "Season.") {
		return key
	}
	return reKeyNonWord.ReplaceAllString(strings.ToLower(filepath.Base(filepath.Dir(season))), "") + key[i:]
}

// rekeyEpisodes moves tv_episodes and episode_gaps to folder keys, once. Keys used to
// drop the year, so ONE PIECE (2023) and ONE PIECE (1999) shared "onepiece_s01e01",
// and filing an episode of either deleted the other's.
//
// The migration is recorded by description under version 1000, clear of upstream's
// numbering. Databases migrated before that hold it as version 9, which upstream
// has since given to audio_projections.
const rekeyEpisodesMigration = "key tv episodes by show folder"

func (d *DB) rekeyEpisodes() error {
	var done int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE description = ?`, rekeyEpisodesMigration).Scan(&done); err != nil || done > 0 {
		return err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	moved := 0
	for _, table := range []string{"tv_episodes", "episode_gaps"} {
		rows, err := tx.Query(`SELECT episode_key, file_path FROM ` + table)
		if err != nil {
			return err
		}
		renames, taken := map[string]string{}, map[string]string{}
		for rows.Next() {
			var key, path string
			if err := rows.Scan(&key, &path); err != nil {
				rows.Close()
				return err
			}
			next := folderEpisodeKey(key, path)
			if other, dup := taken[next]; dup {
				rows.Close()
				return fmt.Errorf("rekey %s: %s and %s would both become %s", table, other, key, next)
			}
			taken[next] = key
			if next != key {
				renames[key] = next
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Two passes, so a new key never collides with an old one that has not moved yet.
		for old, next := range renames {
			if _, err := tx.Exec(`UPDATE `+table+` SET episode_key = ? WHERE episode_key = ?`, "\x01"+next, old); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE ` + table + ` SET episode_key = substr(episode_key, 2) WHERE substr(episode_key, 1, 1) = char(1)`); err != nil {
			return err
		}
		moved += len(renames)
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (1000, ?)`, rekeyEpisodesMigration); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if d.logger != nil {
		d.logger.Printf("[StateDB] Rekeyed %d TV episode entries by show folder", moved)
	}
	return nil
}
