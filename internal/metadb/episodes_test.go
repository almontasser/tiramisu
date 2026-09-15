package metadb

import (
	"path/filepath"
	"testing"
)

func TestRekeyEpisodesByShowFolder(t *testing.T) {
	d, err := New(filepath.Join(t.TempDir(), "t.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	root := "/mnt/tiramisu-mkv-real/"
	for key, path := range map[string]string{
		"onepiece_s01e09": root + "anime/ONE_PIECE (1999)/Season.01/ONE_PIECE_S01E09_3e321958.mkv",
		"onepiece_s02e01": root + "tv/ONE_PIECE (2023)/Season.02/ONE_PIECE_S02E01_1575eafa.mkv",
		"odd_s01e01":      root + "tv/odd.mkv",
	} {
		if err := d.UpsertEpisode(key, EpisodeEntry{Hash: "h", FilePath: path, Source: "api"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.db.Exec(`INSERT INTO episode_gaps (episode_key, season, file_path, dead_hash, removed_at)
		VALUES ('onepiece_s01e05', 1, ?, 'h', 1)`, root+"anime/ONE_PIECE (1999)/Season.01/ONE_PIECE_S01E05_x.mkv"); err != nil {
		t.Fatal(err)
	}
	// New already ran the migration on an empty registry; run it again on these rows.
	if _, err := d.db.Exec(`DELETE FROM schema_version WHERE version = 9`); err != nil {
		t.Fatal(err)
	}
	if err := d.rekeyEpisodes(); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"onepiece1999_s01e09", "onepiece2023_s02e01", "odd_s01e01"} {
		if _, ok, err := d.GetEpisode(key); err != nil || !ok {
			t.Errorf("episode %s missing after rekey (err %v)", key, err)
		}
	}
	var gap string
	if err := d.db.QueryRow(`SELECT episode_key FROM episode_gaps`).Scan(&gap); err != nil || gap != "onepiece1999_s01e05" {
		t.Errorf("gap key = %q (err %v)", gap, err)
	}

	// Version 9 is recorded, so a restart leaves keys written since alone.
	if err := d.UpsertEpisode("onepiece_s09e09", EpisodeEntry{FilePath: root + "tv/X (2020)/Season.09/x.mkv"}); err != nil {
		t.Fatal(err)
	}
	if err := d.rekeyEpisodes(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.GetEpisode("onepiece_s09e09"); !ok {
		t.Error("a second run rekeyed again")
	}
}
