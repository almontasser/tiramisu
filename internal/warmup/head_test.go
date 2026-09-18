package warmup

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeadGateReportsAStubOnceItsHeadIsCached(t *testing.T) {
	d := &DiskWarmupCache{dir: t.TempDir(), writeCh: make(chan warmupWrite, 32)}
	go d.writeWorker()
	oldCache, oldFetch := DiskWarmup, Fetch
	oldStall, oldPause, oldRetry := headStall, headPause, headRetryMin
	DiskWarmup = d
	headStall, headPause, headRetryMin = 50*time.Millisecond, time.Millisecond, 10*time.Millisecond
	defer func() {
		DiskWarmup, Fetch = oldCache, oldFetch
		headStall, headPause, headRetryMin = oldStall, oldPause, oldRetry
	}()

	var swarmDown atomic.Bool
	swarmDown.Store(true)
	Fetch = func(hash string, fileID int, off int64, buf []byte) (int, error) {
		if hash == "dead" && swarmDown.Load() {
			return 0, errors.New("no peers")
		}
		for i := range buf {
			buf[i] = byte((off + int64(i)) % 251)
		}
		return len(buf), nil
	}
	stubs := map[string]string{"/lib/film.mkv": "live", "/lib/stalled.mkv": "dead"}
	reported := make(chan string, 4)
	g := NewHeadGate(func(p string) { reported <- p },
		func(p string) (string, int, bool) { h, ok := stubs[p]; return h, 1, ok },
		func(string, int) {})
	next := func() string {
		select {
		case p := <-reported:
			return p
		case <-time.After(5 * time.Second):
			return "nothing"
		}
	}

	g.Changed("/lib/removed")
	g.Changed("/lib/stalled.mkv")
	g.Changed("/lib/film.mkv")
	if p := next(); p != "/lib/removed" {
		t.Fatalf("first report %q, want the removal at once", p)
	}
	if p := next(); p != "/lib/film.mkv" {
		t.Fatalf("second report %q, want the film once its head is cached", p)
	}

	head := make([]byte, 4096)
	off := ProbeHeadSize - int64(len(head))
	if n, _ := d.ReadAt("live", 1, head, off); n != len(head) || head[0] != byte(off%251) || head[len(head)-1] != byte((ProbeHeadSize-1)%251) {
		t.Fatalf("head cache returned %d bytes at %d that don't match the file", n, off)
	}

	select {
	case p := <-reported:
		t.Fatalf("%q reported while its torrent gave no data", p)
	case <-time.After(200 * time.Millisecond):
	}
	swarmDown.Store(false)
	if p := next(); p != "/lib/stalled.mkv" {
		t.Fatalf("third report %q, want the stalled stub once its swarm answers", p)
	}
}

func TestHeadGateRefillsAHeadTheReaperDroppedMidStall(t *testing.T) {
	d := &DiskWarmupCache{dir: t.TempDir(), writeCh: make(chan warmupWrite, 32)}
	go d.writeWorker()
	oldCache, oldFetch := DiskWarmup, Fetch
	oldStall, oldPause := headStall, headPause
	DiskWarmup = d
	headStall, headPause = 5*time.Second, time.Millisecond
	defer func() {
		DiskWarmup, Fetch = oldCache, oldFetch
		headStall, headPause = oldStall, oldPause
	}()

	// The swarm hands over half a chunk, below the ready floor, then stalls.
	var calls, failed atomic.Int64
	var down atomic.Bool
	down.Store(true)
	Fetch = func(hash string, fileID int, off int64, buf []byte) (int, error) {
		if calls.Add(1) > 1 && down.Load() {
			failed.Add(1)
			return 0, errors.New("no peers")
		}
		n := len(buf)
		if off == 0 && calls.Load() == 1 {
			n = headReadyFloor / 2
		}
		for i := range buf[:n] {
			buf[i] = byte((off + int64(i)) % 251)
		}
		return n, nil
	}
	reported := make(chan string, 1)
	g := NewHeadGate(func(p string) { reported <- p },
		func(string) (string, int, bool) { return "slow", 1, true },
		func(string, int) {})
	g.Changed("/lib/slow.mkv")

	waitFor := func(what string, cond func() bool) {
		for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	waitFor("the short chunk to land", func() bool { return d.GetAvailableRange("slow", 1) == headReadyFloor/2 })
	d.reapIdle(0)
	if _, err := os.Stat(d.filePath("slow", 1)); !os.IsNotExist(err) {
		t.Fatalf("the reaper kept a head below the ready floor: %v", err)
	}
	// Let the fill go round its error path at least once after the drop.
	since := failed.Load()
	waitFor("the fill to retry", func() bool { return failed.Load() >= since+2 })
	down.Store(false)

	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("the stub was never reported: the fill kept writing past the dropped head")
	}
	head := make([]byte, 4096)
	if n, _ := d.ReadAt("slow", 1, head, 0); n != len(head) || head[len(head)-1] != byte((len(head)-1)%251) {
		t.Fatalf("head cache returned %d bytes at 0 that don't match the file", n)
	}
}
