package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// blockingSyncer runs until it is stopped.
type blockingSyncer struct{ name string }

func (b blockingSyncer) Name() string { return b.name }

func (b blockingSyncer) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func newTestScheduler(t *testing.T, cfg SchedulerConfig, names ...string) *Scheduler {
	t.Helper()
	jobs := map[string]Syncer{}
	for _, n := range names {
		jobs[n] = blockingSyncer{n}
	}
	// Not t.TempDir: a stopped job saves its state from its own goroutine, and
	// TempDir's cleanup fails the test when that write lands during removal.
	dir, err := os.MkdirTemp("", "scheduler-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return New(func() SchedulerConfig { return cfg }, jobs, filepath.Join(dir, "state.json"))
}

// stop stops a job once its run has registered, and waits for the run to end.
func stop(t *testing.T, s *Scheduler, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.StopJob(name) != nil {
		if time.Now().After(deadline) {
			t.Errorf("%s never started", name)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	for s.Status()[name].Running || s.Status()[name].LastStatus == "" {
		if time.Now().After(deadline) {
			t.Errorf("%s never stopped", name)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIntervalJobRunsWhenDue(t *testing.T) {
	cfg := SchedulerConfig{Enabled: true, TVSync: DailyJobConfig{Enabled: true, IntervalHours: 1}}
	if !shouldRun(cfg, "tv", JobState{}) {
		t.Error("a job that never ran should be due")
	}
	if shouldRun(cfg, "tv", JobState{LastRun: time.Now().Add(-30 * time.Minute)}) {
		t.Error("a job that ran 30 minutes ago should not be due")
	}
	if !shouldRun(cfg, "tv", JobState{LastRun: time.Now().Add(-61 * time.Minute)}) {
		t.Error("a job that ran 61 minutes ago should be due")
	}
}

// The control panel used to refuse two jobs on one weekday. The scheduler never did.
func TestDailyJobsShareADay(t *testing.T) {
	job := DailyJobConfig{Enabled: true, DaysOfWeek: []int{int(time.Now().Weekday())}}
	cfg := SchedulerConfig{Enabled: true, MoviesSync: job, TVSync: job, AnimeSync: job}
	for _, name := range []string{"movies", "tv", "anime"} {
		if !shouldRun(cfg, name, JobState{}) {
			t.Errorf("%s should be due today", name)
		}
	}
}

// The TV and anime jobs are one engine over two trees; a run's cleanup would
// delete what the other run just filed.
func TestTVAndAnimeNeverOverlap(t *testing.T) {
	s := newTestScheduler(t, SchedulerConfig{}, "tv", "anime", "movies")

	if err := s.TriggerRun("tv"); err != nil {
		t.Fatal(err)
	}
	if err := s.TriggerRun("anime"); !errors.Is(err, ErrBusy) {
		t.Errorf("anime started beside tv: %v", err)
	}
	if err := s.TriggerRun("movies"); err != nil {
		t.Errorf("movies should run beside tv: %v", err)
	}
	stop(t, s, "tv")
	stop(t, s, "movies")
}

func TestTickDefersTheOverlappingJob(t *testing.T) {
	due := DailyJobConfig{Enabled: true, IntervalHours: 1}
	s := newTestScheduler(t, SchedulerConfig{Enabled: true, TVSync: due, AnimeSync: due}, "tv", "anime")

	s.tick()
	status := s.Status()
	if status["tv"].Running == status["anime"].Running {
		t.Errorf("exactly one of tv and anime should be running: %+v", status)
	}
	for name, st := range status {
		if st.Running {
			stop(t, s, name)
		}
	}
}

func TestDisabledSchedulerStartsNothing(t *testing.T) {
	due := DailyJobConfig{Enabled: true, IntervalHours: 1}
	s := newTestScheduler(t, SchedulerConfig{Enabled: false, MoviesSync: due}, "movies")

	s.tick()
	if s.Status()["movies"].Running {
		stop(t, s, "movies")
		t.Error("a disabled scheduler started a job")
	}
}
