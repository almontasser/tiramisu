package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrAlreadyRunning is returned when TriggerRun is called on a job that is already running.
var ErrAlreadyRunning = errors.New("job already running")

// ErrBusy is returned when TriggerRun is called on a job that must not run
// alongside one that is running.
var ErrBusy = errors.New("cannot run while another job of its group is running")

// JobState tracks the runtime state of a scheduled job.
type JobState struct {
	LastRun    time.Time `json:"last_run,omitempty"`
	NextRun    time.Time `json:"next_run,omitempty"`
	Running    bool      `json:"running"`
	LastError  string    `json:"last_error,omitempty"`
	LastStatus string    `json:"last_status,omitempty"`
}

// Syncer is the interface all sync engines must implement.
type Syncer interface {
	Name() string
	Run(ctx context.Context) error
}

// ErrNotRunning is returned when StopJob is called on a job that is not running.
var ErrNotRunning = errors.New("job not running")

// exclusionGroups names the jobs that must never run at the same time. The TV
// and anime jobs are one engine over two trees, and each run's cleanup deletes
// the stubs and torrents that the registry it loaded does not list, which would
// include the other run's newest.
var exclusionGroups = map[string]string{"tv": "series", "anime": "series"}

// Scheduler manages scheduled and manual sync jobs.
type Scheduler struct {
	cfg     func() SchedulerConfig
	jobs    map[string]Syncer
	state   *StateStore
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// SchedulerConfig mirrors the config.go struct to avoid import cycles.
type SchedulerConfig struct {
	Enabled       bool
	MoviesSync    DailyJobConfig
	TVSync        DailyJobConfig
	AnimeSync     DailyJobConfig
	WatchlistSync WatchlistSyncConfig
}

// DailyJobConfig mirrors config.go.
type DailyJobConfig struct {
	Enabled    bool
	DaysOfWeek []int
	Hour       int
	Minute     int
	// IntervalHours, above zero, runs the job every that many hours instead of
	// on DaysOfWeek at Hour:Minute.
	IntervalHours int
}

// WatchlistSyncConfig mirrors config.go.
type WatchlistSyncConfig struct {
	Enabled       bool
	IntervalHours int
}

// New creates a Scheduler. cfg is read on every tick, so a schedule saved from
// the control panel applies within a minute, without a restart.
func New(cfg func() SchedulerConfig, jobs map[string]Syncer, statePath string) *Scheduler {
	ss, _ := NewStateStore(statePath)

	s := &Scheduler{
		cfg:     cfg,
		jobs:    jobs,
		state:   ss,
		cancels: make(map[string]context.CancelFunc),
	}

	// Pre-populate trackers for all jobs and reset stale running state
	for name := range jobs {
		jt := ss.Tracker(name)
		jt.SetRunning(false) // Reset stale running state from previous run
	}

	// Calculate initial NextRun times
	s.updateNextRuns(cfg())

	return s
}

// Run starts the scheduler loop. Blocks until stop is closed. Jobs start on
// their own only while the configuration has the scheduler enabled.
func (s *Scheduler) Run(stop <-chan struct{}) {
	logger := log.New(os.Stdout, "[Scheduler] ", log.LstdFlags)
	logger.Printf("started (tick=60s)")

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			logger.Printf("stopping")
			s.state.Save()
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// TriggerRun starts a job immediately. It returns ErrAlreadyRunning if the job
// is already running, and ErrBusy if a job it must not overlap is.
func (s *Scheduler) TriggerRun(name string) error {
	syncer, ok := s.jobs[name]
	if !ok {
		return fmt.Errorf("unknown job: %s", name)
	}

	jt := s.state.Tracker(name)
	s.mu.Lock()
	if jt.Snapshot().Running {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	if other := s.conflicting(name); other != "" {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrBusy, other)
	}
	// V2.0: Set running before spawn to prevent concurrent launches from tick().
	jt.SetRunning(true)
	s.mu.Unlock()

	s.state.Save()
	go s.runJob(syncer, jt)
	return nil
}

// StopJob cancels a running job. Returns ErrNotRunning if the job is not running.
func (s *Scheduler) StopJob(name string) error {
	s.mu.Lock()
	cancel, ok := s.cancels[name]
	s.mu.Unlock()

	if !ok {
		return ErrNotRunning
	}

	cancel()
	log.Printf("[Scheduler] %s stop requested", name)
	return nil
}

// Status returns a snapshot of all job states.
func (s *Scheduler) Status() map[string]JobState {
	return s.state.Status()
}

// conflicting returns a running job that name must not overlap, or "". The
// caller holds s.mu.
func (s *Scheduler) conflicting(name string) string {
	group := exclusionGroups[name]
	if group == "" {
		return ""
	}
	for other := range s.jobs {
		if other != name && exclusionGroups[other] == group && s.state.Tracker(other).Snapshot().Running {
			return other
		}
	}
	return ""
}

func (s *Scheduler) tick() {
	cfg := s.cfg()
	if cfg.Enabled {
		names := make([]string, 0, len(s.jobs))
		for name := range s.jobs {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			jt := s.state.Tracker(name)
			if !shouldRun(cfg, name, jt.Snapshot()) {
				continue
			}

			s.mu.Lock()
			if s.conflicting(name) != "" {
				// Still due on the next tick, so it starts once the other finishes.
				s.mu.Unlock()
				continue
			}
			// V2.0: Set running before spawn to prevent concurrent TriggerRun/tick races.
			jt.SetRunning(true)
			s.mu.Unlock()
			go s.runJob(s.jobs[name], jt)
		}
	}

	s.updateNextRuns(cfg)
	s.state.Save()
}

// jobConfig is the schedule of one job. The watchlist job only ever runs on an
// interval, so its config is expressed as an interval schedule.
func jobConfig(cfg SchedulerConfig, name string) (DailyJobConfig, bool) {
	switch name {
	case "movies":
		return cfg.MoviesSync, true
	case "tv":
		return cfg.TVSync, true
	case "anime":
		return cfg.AnimeSync, true
	case "watchlist":
		return DailyJobConfig{Enabled: cfg.WatchlistSync.Enabled, IntervalHours: cfg.WatchlistSync.IntervalHours}, true
	}
	return DailyJobConfig{}, false
}

func shouldRun(cfg SchedulerConfig, name string, state JobState) bool {
	if state.Running {
		return false
	}
	job, ok := jobConfig(cfg, name)
	if !ok {
		return false
	}
	if job.IntervalHours > 0 {
		return shouldRunInterval(state, job.Enabled, job.IntervalHours)
	}
	return shouldRunDaily(state, job.Enabled, job.DaysOfWeek, job.Hour, job.Minute)
}

func shouldRunDaily(state JobState, enabled bool, daysOfWeek []int, hour, minute int) bool {
	if !enabled {
		return false
	}

	now := time.Now()
	if !state.NextRun.IsZero() && now.Before(state.NextRun) {
		return false
	}

	daySet := make(map[int]bool)
	for _, d := range daysOfWeek {
		daySet[d] = true
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !daySet[int(today.Weekday())] {
		return false
	}

	if now.Before(today) {
		return false
	}

	if !state.LastRun.IsZero() {
		lastDay := time.Date(state.LastRun.Year(), state.LastRun.Month(), state.LastRun.Day(), 0, 0, 0, 0, now.Location())
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		if !lastDay.Before(todayStart) {
			return false
		}
	}

	return true
}

// shouldRunInterval measures the interval from the end of the last run, so a
// run that takes longer than its interval is followed by the next one at once,
// never overlapped by it.
func shouldRunInterval(state JobState, enabled bool, intervalHours int) bool {
	if !enabled || intervalHours <= 0 {
		return false
	}

	if state.LastRun.IsZero() {
		return true
	}

	return time.Since(state.LastRun) >= time.Duration(intervalHours)*time.Hour
}

func (s *Scheduler) runJob(syncer Syncer, jt *JobTracker) {
	name := syncer.Name()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Scheduler] %s PANIC: %v", name, r)
			jt.SetRunning(false)
			jt.SetStatus("failed", fmt.Sprintf("panic: %v", r))
			s.state.Save()
		}
		s.mu.Lock()
		delete(s.cancels, name)
		s.mu.Unlock()
	}()

	s.state.Save()
	log.Printf("[Scheduler] %s started", name)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.mu.Lock()
	s.cancels[name] = cancel
	s.mu.Unlock()

	err := syncer.Run(ctx)

	jt.SetRunning(false)
	if err != nil && ctx.Err() == nil {
		log.Printf("[Scheduler] %s failed: %v", name, err)
		jt.SetStatus("failed", err.Error())
	} else if ctx.Err() != nil {
		log.Printf("[Scheduler] %s stopped by user", name)
		jt.SetStatus("stopped", "")
	} else {
		log.Printf("[Scheduler] %s completed", name)
		jt.SetStatus("ok", "")
	}

	s.state.Save()
}

func (s *Scheduler) updateNextRuns(cfg SchedulerConfig) {
	for name := range s.jobs {
		jt := s.state.Tracker(name)
		state := jt.Snapshot()

		var next time.Time
		if job, ok := jobConfig(cfg, name); ok && job.Enabled {
			if job.IntervalHours > 0 {
				next = state.LastRun.Add(time.Duration(job.IntervalHours) * time.Hour)
			} else {
				next = nextRunTime(job.Enabled, job.DaysOfWeek, job.Hour, job.Minute)
			}
		}

		if !next.Equal(state.NextRun) {
			jt.SetNextRun(next)
		}
	}
}

func nextRunTime(enabled bool, daysOfWeek []int, hour, minute int) time.Time {
	if !enabled {
		return time.Time{}
	}

	now := time.Now()
	daySet := make(map[int]bool)
	for _, d := range daysOfWeek {
		daySet[d] = true
	}

	for offset := 0; offset < 8; offset++ {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location()).AddDate(0, 0, offset)
		if offset == 0 {
			if candidate.After(now) && daySet[int(candidate.Weekday())] {
				return candidate
			}
			continue
		}
		if daySet[int(candidate.Weekday())] {
			return candidate
		}
	}

	return time.Time{}
}

// --- StateStore (embedded to avoid import cycles with engines) ---

// stateFile is the on-disk format.
type stateFile struct {
	Jobs map[string]*JobState `json:"jobs"`
}

// StateStore persists JobState to a JSON file with atomic writes.
type StateStore struct {
	path string
	mu   sync.Mutex
	jobs map[string]*JobTracker
}

// NewStateStore loads or creates the state file.
func NewStateStore(path string) (*StateStore, error) {
	ss := &StateStore{
		path: path,
		jobs: make(map[string]*JobTracker),
	}

	if data, err := os.ReadFile(path); err == nil {
		var sf stateFile
		if err := json.Unmarshal(data, &sf); err == nil {
			for name, js := range sf.Jobs {
				ss.jobs[name] = &JobTracker{state: *js}
			}
		}
	}

	return ss, nil
}

// Tracker returns the JobTracker for a named job, creating if needed.
func (ss *StateStore) Tracker(name string) *JobTracker {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if jt, ok := ss.jobs[name]; ok {
		return jt
	}
	jt := &JobTracker{}
	ss.jobs[name] = jt
	return jt
}

// Status returns a snapshot of all job states.
func (ss *StateStore) Status() map[string]JobState {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	result := make(map[string]JobState)
	for name, jt := range ss.jobs {
		result[name] = jt.Snapshot()
	}
	return result
}

// Save persists all job states to disk atomically.
func (ss *StateStore) Save() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	sf := stateFile{Jobs: make(map[string]*JobState)}
	for name, jt := range ss.jobs {
		js := jt.Snapshot()
		sf.Jobs[name] = &js
	}

	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmp := ss.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write state tmp: %w", err)
	}

	return os.Rename(tmp, ss.path)
}

// JobTracker is a thread-safe view of a single job's state.
type JobTracker struct {
	mu    sync.Mutex
	state JobState
}

// SetRunning updates the running flag.
func (jt *JobTracker) SetRunning(running bool) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	jt.state.Running = running
}

// SetStatus updates the last status and error.
func (jt *JobTracker) SetStatus(status, errMsg string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	jt.state.LastStatus = status
	jt.state.LastError = errMsg
	jt.state.LastRun = time.Now()
}

// SetNextRun updates the next scheduled run time.
func (jt *JobTracker) SetNextRun(next time.Time) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	jt.state.NextRun = next
}

// Snapshot returns a copy of the current state.
func (jt *JobTracker) Snapshot() JobState {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	return jt.state
}
