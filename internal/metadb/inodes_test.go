package metadb

import (
	"fmt"
	"path/filepath"
	"testing"
)

// The jf library holds about 34k files, past SQLite's 32766-variable limit, so the
// prune must not bind one variable per valid file.
func TestPruneMissingPastVariableLimit(t *testing.T) {
	d, err := New(filepath.Join(t.TempDir(), "t.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	const n = 40000
	tx, err := d.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	valid := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		fp := fmt.Sprintf("/mnt/tv/show/e%05d.mkv", i)
		if _, err := tx.Exec(`INSERT INTO inodes (type, infohash, file_idx, full_path, basename, inode_value)
			VALUES ('file', 'h', ?, ?, ?, ?)`, i, fp, pathBase(fp), i+1); err != nil {
			t.Fatal(err)
		}
		if i%10000 != 0 {
			valid[fp] = true
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertDir("tv/show", 1<<63|7); err != nil {
		t.Fatal(err)
	}

	pruned, err := d.PruneMissing(valid)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 4 {
		t.Errorf("pruned %d, want 4", pruned)
	}
	var files, dirs int
	d.db.QueryRow("SELECT COUNT(*) FROM inodes WHERE type = 'file'").Scan(&files)
	d.db.QueryRow("SELECT COUNT(*) FROM inodes WHERE type = 'dir'").Scan(&dirs)
	if files != n-4 || dirs != 1 {
		t.Errorf("left %d files and %d dirs, want %d and 1", files, dirs, n-4)
	}
	if _, ok, _ := d.GetFileInode("/mnt/tv/show/e10000.mkv"); ok {
		t.Error("e10000 survived the prune")
	}
	if _, ok, _ := d.GetFileInode("/mnt/tv/show/e10001.mkv"); !ok {
		t.Error("e10001 was pruned")
	}
}
