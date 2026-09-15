package library

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEpisodeKeySeparatesRemakes(t *testing.T) {
	anime := EpisodeKey("One Piece", "1999-10-20", 1, 1)
	if anime != "onepiece1999_s01e01" {
		t.Fatalf("EpisodeKey = %q", anime)
	}
	if live := EpisodeKey("ONE PIECE", "2023-08-31", 1, 1); live == anime {
		t.Fatalf("the 2023 series shares the 1999 series' key %q", live)
	}
	// The sync rebuilds keys from the folder on disk; it must land on the same key.
	if fromFolder := EpisodeKey("ONE_PIECE (1999)", "", 1, 1); fromFolder != anime {
		t.Fatalf("folder key %q != title key %q", fromFolder, anime)
	}
}

func TestShowDirReusesFolderInOtherCase(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "ONE_PIECE (1999)"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ShowDir(root, "One Piece", "1999-10-20"); got != filepath.Join(root, "ONE_PIECE (1999)") {
		t.Fatalf("ShowDir = %q", got)
	}
	if got := ShowDir(root, "Shogun", "2024"); got != filepath.Join(root, "Shogun (2024)") {
		t.Fatalf("ShowDir for a new show = %q", got)
	}
}
