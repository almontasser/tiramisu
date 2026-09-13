package library

import (
	"context"
	"time"
)

const defaultRefreshDelay = 15 * time.Second

// MediaServer is the Plex/Jellyfin library refresh. mediaserver.Client implements it.
type MediaServer interface {
	RefreshLibrary(ctx context.Context, sectionID int) error
}

// kindRefresher is a media server that can refresh the single library holding
// one kind of content. mediaserver.JellyfinClient is one.
type kindRefresher interface {
	RefreshKind(ctx context.Context, kind string) error
}

// libraryKind is the library an add of kind lands in: "movies", "tv" or "anime".
func libraryKind(kind string) string {
	switch {
	case kind == "anime":
		return "anime"
	case isSeriesKind(kind):
		return "tv"
	default:
		return "movies"
	}
}

// scheduleRefresh asks the media server to rescan the library an add of kind
// lands in, coalescing the requests of a burst of adds into one: a client filing
// a whole filmography must not make Plex rescan once per title. A stub is
// invisible to the media server until it rescans, so this is part of the add,
// not an extra.
func (m *Manager) scheduleRefresh(kind string) {
	if m.cfg.MediaServer == nil {
		return
	}
	delay := m.cfg.RefreshDelay
	if delay <= 0 {
		delay = defaultRefreshDelay
	}
	library := libraryKind(kind)

	m.mu.Lock()
	if m.refreshPending == nil {
		m.refreshPending = map[string]bool{}
		m.refreshDirty = map[string]bool{}
	}
	if m.refreshPending[library] {
		// A scan for this library is already coming. If it is the one being waited
		// for, it will cover this request too; if it is already running, it cannot,
		// so mark the library dirty and let it schedule another when it finishes.
		// Dropping the request is how a remove and the add that replaces it end up
		// sharing one scan that saw only half the change.
		m.refreshDirty[library] = true
		m.mu.Unlock()
		return
	}
	m.refreshPending[library] = true
	m.mu.Unlock()

	go func() {
		time.Sleep(delay)

		// Requests that arrived during the wait are covered by the scan below.
		m.mu.Lock()
		delete(m.refreshDirty, library)
		m.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var err error
		if kr, ok := m.cfg.MediaServer.(kindRefresher); ok {
			err = kr.RefreshKind(ctx, library)
		} else {
			err = m.cfg.MediaServer.RefreshLibrary(ctx, m.section(kind))
		}

		m.mu.Lock()
		delete(m.refreshPending, library)
		again := m.refreshDirty[library]
		delete(m.refreshDirty, library)
		m.mu.Unlock()

		if err != nil {
			m.cfg.Logger.Printf("[LibraryAPI] WARNING: library refresh failed: %v", err)
		}
		if again {
			m.scheduleRefresh(kind)
		}
	}()
}
