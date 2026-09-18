package warmup

import (
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// ProbeHeadSize is how much of a new file HeadGate caches before the media server hears of
// it. Jellyfin probes with ffprobe -show_streams -show_format, which on 2026-09-15 read 1.2
// to 7.7MB from the start of each file and nothing from the end.
const ProbeHeadSize int64 = 16 << 20

// HeadGate tuning, in vars so the test need not wait it out.
var (
	headFills    = 4               // head fills running at once, across all torrents
	headStall    = 2 * time.Minute // a fill that gets no bytes for this long gives up
	headPause    = time.Second     // wait between failed fetches of a fill
	headRetryMin = time.Minute     // wait before retrying a torrent that gave no bytes
	headRetryMax = time.Hour
)

// HeadGate holds back the media server's report of a new stub until the head of its file is
// on SSD. Jellyfin refreshes one folder after another and probes each new file in turn, and
// the probe of an uncached file waits on the swarm: on 2026-09-15 a 273-episode pack took two
// minutes an episode, and a movie and 175 TV episodes filed after it waited hours. With the
// head cached, the probe reads the SSD, and a slow torrent delays only its own files.
//
// The files of one torrent are filled in turn, and different torrents in parallel. A pass
// over a torrent's files stops at the first one that gets no bytes and retries later, with a
// growing wait, so a dead torrent holds a slot for one stall per pass. Its stubs stay
// unreported until the swarm answers or the stubs are removed.
//
// ponytail: waiting fills live in memory, so a restart drops them, and those stubs reach
// Jellyfin only through its scheduled library scan. Persist the queue if that bites.
type HeadGate struct {
	report func(path string)
	stub   func(path string) (hash string, fileID int, ok bool)
	wake   func(hash string, fileID int)
	slots  chan struct{}

	mu    sync.Mutex
	queue map[string][]string // info hash -> stub paths waiting on its fill
}

// NewHeadGate returns a gate that passes reports on to report. stub names the torrent file
// behind a stub, ok false when path is not a readable stub; wake loads a torrent so Fetch can
// read it.
func NewHeadGate(report func(string), stub func(string) (string, int, bool), wake func(string, int)) *HeadGate {
	return &HeadGate{report: report, stub: stub, wake: wake, slots: make(chan struct{}, headFills), queue: map[string][]string{}}
}

// Changed takes the report of a stub or directory written or removed. A stub whose head is
// not cached yet is reported once it is; everything else is reported now.
func (g *HeadGate) Changed(path string) {
	hash, fileID, ok := g.stub(path)
	if !ok || DiskWarmup == nil || Fetch == nil || DiskWarmup.GetAvailableRange(hash, fileID) >= ProbeHeadSize {
		g.report(path)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	waiting, running := g.queue[hash]
	if slices.Contains(waiting, path) {
		return
	}
	g.queue[hash] = append(waiting, path)
	if !running {
		go g.run(hash)
	}
}

// run fills the heads of the stubs waiting on one torrent, reporting each as it lands, and
// returns once none is left.
func (g *HeadGate) run(hash string) {
	wait := headRetryMin
	for {
		g.mu.Lock()
		paths := slices.Clone(g.queue[hash])
		if len(paths) == 0 {
			delete(g.queue, hash)
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()

		for _, p := range paths {
			h, fileID, ok := g.stub(p)
			if !ok || h != hash {
				// Removed, or replaced by another release, whose own report queued it.
				g.drop(hash, p)
				continue
			}
			started := time.Now()
			got, err := g.fill(hash, fileID)
			took := time.Since(started).Truncate(time.Second)
			if err == nil {
				logf.Printf("[DiskWarmup] HEAD READY %s (%.1fMB fetched in %s)", filepath.Base(p), float64(got)/(1<<20), took)
				g.drop(hash, p)
				wait = headRetryMin
				g.report(p)
				continue
			}
			logf.Printf("[DiskWarmup] HEAD NOT READY %s (%.1fMB fetched in %s): %v", filepath.Base(p), float64(got)/(1<<20), took, err)
			if got == 0 {
				// The torrent gave nothing, and its other files would fare no better now.
				time.Sleep(wait)
				wait = min(2*wait, headRetryMax)
				break
			}
		}
	}
}

func (g *HeadGate) drop(hash, path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.queue[hash] = slices.DeleteFunc(g.queue[hash], func(p string) bool { return p == path })
}

// fill caches the first ProbeHeadSize bytes of one file, resuming from what is on SSD. It
// holds a slot while it runs, and returns the bytes it fetched, with nil once they landed.
func (g *HeadGate) fill(hash string, fileID int) (int64, error) {
	g.slots <- struct{}{}
	defer func() { <-g.slots }()

	d := DiskWarmup
	want := min(ProbeHeadSize, FileSize)
	g.wake(hash, fileID)
	// Points the torrent's peer management at this file's first pieces, as a pump warmup does.
	if OnWarmupStateChange != nil {
		OnWarmupStateChange(hash, fileID, true)
		defer OnWarmupStateChange(hash, fileID, false)
	}

	var got int64
	buf := make([]byte, tailFillChunk)
	progress := time.Now()
	for off := d.GetAvailableRange(hash, fileID); off < want; {
		n, err := Fetch(hash, fileID, off, buf[:min(int64(len(buf)), want-off)])
		if err == nil && n <= 0 {
			err = errors.New("fetch returned no data")
		}
		if err != nil {
			if time.Since(progress) >= headStall {
				return got, err
			}
			time.Sleep(headPause)
			g.wake(hash, fileID) // reloads a torrent that closed; a no-op while it runs
			// A stall long enough for the reaper drops a head still under the ready floor
			// (dropResidue), and processWrite refuses every write past the hole it leaves.
			off = min(off, d.GetAvailableRange(hash, fileID))
			continue
		}
		d.enqueue(hash, fileID, buf[:n], off, true)
		off += int64(n)
		got += int64(n)
		progress = time.Now()
	}
	// The worker writes in the background, so wait for the coverage to show the writes.
	for deadline := time.Now().Add(10 * time.Second); d.GetAvailableRange(hash, fileID) < want; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			return got, errors.New("the head writes did not land")
		}
	}
	return got, nil
}
