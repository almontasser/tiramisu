package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// An eleven-minute episode is under 100 MB and must still reach the mount.
func TestShortEpisodeStubIsValid(t *testing.T) {
	dir := t.TempDir()
	stub := func(size string) string {
		p := filepath.Join(dir, size+".mkv")
		os.WriteFile(p, []byte(`{"url":"http://127.0.0.1:8090/stream?link=f8b0fc1a&index=1&play","size":`+size+`}`), 0o644)
		return p
	}
	if m, err := ReadMetadataFromFile(stub("85025085")); err != nil || m.Size != 85025085 {
		t.Fatalf("85 MB stub: %+v, %v", m, err)
	}
	if _, err := ReadMetadataFromFile(stub("0")); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("empty stub: err = %v, want ErrInvalidSize", err)
	}
}
