package library

import (
	"os"
	"path/filepath"
	"syscall"
)

// Ownership of everything this package writes to the source tree.
//
// The process itself has to be root: mounting FUSE needs CAP_SYS_ADMIN, and
// Docker grants capabilities to the container's root user only, so dropping
// privileges would cost the mount. Nothing else about Tiramisu wants to be
// root, though, and leaving the files it writes owned by root:root has a real
// cost — the media server runs as an ordinary user and so does whoever owns the
// data directory, and neither can then remove a stub or prune an emptied show
// directory without a privileged container.
//
// So the process stays root and the files do not.
var (
	ownerUID = -1
	ownerGID = -1
)

// SetOwner fixes who new stubs and directories belong to. A non-positive uid or
// gid means "take it from dir", which is almost always the right answer: the
// source tree is bind-mounted from the host and already carries the ownership
// the host expects.
func SetOwner(uid, gid int, dir string) {
	if uid > 0 && gid > 0 {
		ownerUID, ownerGID = uid, gid
		return
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	ownerUID, ownerGID = int(st.Uid), int(st.Gid)
}

// Owner reports the configured ownership, for logging. -1 means unset.
func Owner() (int, int) { return ownerUID, ownerGID }

// chownPath applies the configured ownership. Failures are ignored on purpose:
// a stub that exists with an awkward owner is worth more than no stub, and the
// call is a no-op when the process already runs as that user.
func chownPath(path string) {
	if ownerUID < 0 || ownerGID < 0 {
		return
	}
	_ = os.Chown(path, ownerUID, ownerGID)
}

// ApplyOwner fixes the owner of a path written outside this package. Exported
// for the watchlist engine, which assembles its stub inline instead of going
// through WriteStub.
func ApplyOwner(path string) { chownPath(path) }

// mkdirAllOwned is os.MkdirAll that also fixes the owner of every level it had
// to create.
//
// Chowning only the leaf is not enough, and the reason is the one that bites:
// permission to delete a file comes from write access to the directory holding
// it, not from the file. A season directory owned by the right user inside a
// show directory owned by root still cannot be pruned when the show empties.
func mkdirAllOwned(dir string) error {
	if dir == "" {
		return nil
	}

	// Collect the levels that do not exist yet, before creating them - once
	// MkdirAll has run there is no way to tell which ones it made.
	var created []string
	for p := dir; ; {
		if _, err := os.Stat(p); err == nil {
			break
		}
		created = append(created, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, p := range created {
		chownPath(p)
	}
	return nil
}
