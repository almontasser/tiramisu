package mediaserver

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"tiramisu/internal/library"
)

// pendingTTL is how long the watch state of a removed stub waits for a replacement.
// The reaper drops a dead release and the re-search that refills it can be a run or a
// day later; after a month, nothing is coming.
const pendingTTL = 30 * 24 * time.Hour

// pending is the watch state of a stub that was removed, waiting for the stub that
// takes its place.
type pending struct {
	Path  string                  `json:"path"`
	Users map[string]itemUserData `json:"users"`
	Saved time.Time               `json:"saved"`
}

// pendingStore keeps those on disk, so a Tiramisu restart between the removal and the
// refill does not lose them. Small enough to rewrite whole: an entry per stub removed
// with something watched on it, pruned after pendingTTL.
type pendingStore struct {
	path string
	mu   sync.Mutex
	byID map[string]pending
}

func newPendingStore(path string) *pendingStore {
	s := &pendingStore{path: path, byID: map[string]pending{}}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var loaded map[string]pending
	if json.Unmarshal(data, &loaded) == nil {
		for id, p := range loaded {
			if time.Since(p.Saved) < pendingTTL {
				s.byID[id] = p
			}
		}
	}
	return s
}

func (s *pendingStore) put(id string, p pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[id] = p
	s.pruneLocked()
	s.saveLocked()
}

func (s *pendingStore) take(id string) (pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	return p, ok
}

// all returns a copy of the held entries, for a caller that walks them.
func (s *pendingStore) all() map[string]pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]pending, len(s.byID))
	for id, p := range s.byID {
		out[id] = p
	}
	return out
}

func (s *pendingStore) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
	s.saveLocked()
}

func (s *pendingStore) pruneLocked() {
	for id, p := range s.byID {
		if time.Since(p.Saved) >= pendingTTL {
			delete(s.byID, id)
		}
	}
}

// saveLocked rewrites the file through a temporary one, so a crash mid-write leaves the
// previous list rather than half of this one.
func (s *pendingStore) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.byID)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return
	}
	library.ApplyOwner(s.path)
}
