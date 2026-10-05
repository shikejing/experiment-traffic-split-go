package experiment

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MaxHistoryRecords bounds the in-memory audit trail so long-running processes
// do not grow memory unboundedly.
const MaxHistoryRecords = 50

// Common manager errors.
var (
	ErrNoChange          = errors.New("config version equals active version")
	ErrVersionNotFound   = errors.New("no such config version")
	ErrRolloutInProgress = errors.New("a candidate version is already rolling out; promote or roll it back first")
	ErrNoRollout         = errors.New("no candidate version is being rolled out")
	ErrInvalidPercent    = errors.New("percent must be in [0,100]")
	ErrNoPreviousConfig  = errors.New("no previous stable configuration to restore")
)

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Sink receives assignment events. When nil, event recording is disabled
	// and the assignment path performs no queueing at all.
	Sink EventSink
	// EventQueueSize bounds the asynchronous event buffer (0 => default).
	EventQueueSize int
	// EventSampling in [0,1]; deterministic per (experiment, user).
	EventSampling float64
	// DropPolicy applied when the event buffer is full.
	DropPolicy DropPolicy
	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time
}

// managerState is immutable once published; Evaluate reads it via an atomic
// pointer, so concurrent readers always observe one complete snapshot and
// never a half-updated configuration.
type managerState struct {
	stable       *engine // config served to users not in the rollout gate
	lastStable   *engine // previous stable, for hot-update rollback
	candidate    *engine // version being gradually released; nil when idle
	candidatePct int     // 0..100, share of users routed to candidate
	history      []VersionRecord
}

// Manager is the reusable, concurrency-safe entry point. It owns the active
// configuration, rollout state and the event pipeline.
type Manager struct {
	state atomic.Pointer[managerState]
	opts  ManagerOptions
	now   func() time.Time

	queue *EventQueue // nil when recording disabled

	// mu serializes configuration transitions; it is never held on the
	// Evaluate hot path.
	mu sync.Mutex
}

// NewManager constructs a Manager with an initial validated config. The
// initial config becomes the stable version at 100%.
func NewManager(initial *Config, opts ManagerOptions) (*Manager, error) {
	eng, err := compileEngine(initial)
	if err != nil {
		return nil, fmt.Errorf("invalid initial config: %w", err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	m := &Manager{opts: opts, now: now}
	st := &managerState{
		stable:  eng,
		history: []VersionRecord{{Version: eng.version, Action: "initialize", Percent: 100, Timestamp: now()}},
	}
	m.state.Store(st)

	if opts.Sink != nil {
		m.queue = NewEventQueue(EventQueueOptions{
			Capacity: opts.EventQueueSize,
			Sink:     opts.Sink,
			Policy:   opts.DropPolicy,
			Sampling: opts.EventSampling,
		})
	}
	return m, nil
}

// Evaluate returns assignments for the user from the effective configuration.
// When a gray release is in progress, the user is routed deterministically
// (per user hash) to candidate or stable; the chosen version is recorded in
// Decision.ConfigFrom and each Assignment.ConfigFrom.
func (m *Manager) Evaluate(u User) *Decision {
	st := m.state.Load()
	eng := st.stable
	if st.candidate != nil && st.candidatePct > 0 {
		gate := hashPercent("rollout:" + st.candidate.version + ":" + u.ID)
		if gate < uint32(st.candidatePct) {
			eng = st.candidate
		}
	}
	d := eng.evaluate(u)
	m.emit(u, d)
	return d
}

// Assignment returns the single-experiment assignment (nil if unknown).
func (m *Manager) Assignment(u User, experimentKey string) *Assignment {
	return m.Evaluate(u).Assignments[experimentKey]
}

// Variant returns the variant key for an experiment, or "" when the user is
// not bucketed into it.
func (m *Manager) Variant(u User, experimentKey string) string {
	a := m.Assignment(u, experimentKey)
	if a == nil {
		return ""
	}
	return a.Variant
}

// Update performs an instant, atomic hot reload. The new config is fully
// validated/compiled before it is published: on failure the currently active
// configuration keeps serving untouched (automatic rollback on switch
// failure). The previous stable version is retained and can be restored via
// Rollback(previousVersion).
func (m *Manager) Update(cfg *Config) error {
	eng, err := compileEngine(cfg)
	if err != nil {
		return err // active state untouched
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.state.Load()
	if cur.candidate != nil {
		return ErrRolloutInProgress
	}
	if eng.version == cur.stable.version {
		return ErrNoChange
	}
	next := &managerState{
		stable:       eng,
		lastStable:   cur.stable,
		candidate:    nil,
		candidatePct: 0,
		history:      appendRecord(cur.history, VersionRecord{Version: eng.version, Action: "activate", Percent: 100, Timestamp: m.now()}),
	}
	m.state.Store(next)
	return nil
}

// StageRollout validates and registers a new candidate version at 0%. No user
// is affected until SetRollout raises the percentage. On validation failure
// the active configuration is untouched.
func (m *Manager) StageRollout(cfg *Config) error {
	eng, err := compileEngine(cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.state.Load()
	if cur.candidate != nil {
		return fmt.Errorf("%w: candidate=%s", ErrRolloutInProgress, cur.candidate.version)
	}
	if eng.version == cur.stable.version {
		return ErrNoChange
	}
	next := &managerState{
		stable:       cur.stable,
		lastStable:   cur.lastStable,
		candidate:    eng,
		candidatePct: 0,
		history: appendRecord(cur.history,
			VersionRecord{Version: eng.version, Action: "stage", Percent: 0, Timestamp: m.now()}),
	}
	m.state.Store(next)
	return nil
}

// SetRollout moves the share of users routed to the candidate version to
// percent (0..100). Routing is a deterministic per-user hash, so raising the
// percentage only adds users; users below the previous threshold never move
// back to the old config. Lowering it partially rolls back the newest users.
func (m *Manager) SetRollout(version string, percent int) error {
	if percent < 0 || percent > 100 {
		return ErrInvalidPercent
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.state.Load()
	if cur.candidate == nil {
		return ErrNoRollout
	}
	if cur.candidate.version != version {
		return fmt.Errorf("%w: running candidate=%s, requested=%s", ErrVersionNotFound, cur.candidate.version, version)
	}
	if percent == cur.candidatePct {
		return nil
	}
	next := &managerState{
		stable:       cur.stable,
		lastStable:   cur.lastStable,
		candidate:    cur.candidate,
		candidatePct: percent,
		history: appendRecord(cur.history,
			VersionRecord{Version: version, Action: "rollout", Percent: percent, Timestamp: m.now()}),
	}
	m.state.Store(next)
	return nil
}

// Promote ends a gray release: the candidate becomes the sole stable config
// at 100%. From that point all users use it.
func (m *Manager) Promote(version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.state.Load()
	if cur.candidate == nil {
		return ErrNoRollout
	}
	if cur.candidate.version != version {
		return fmt.Errorf("%w: running candidate=%s, requested=%s", ErrVersionNotFound, cur.candidate.version, version)
	}
	next := &managerState{
		stable:       cur.candidate,
		lastStable:   cur.stable,
		candidate:    nil,
		candidatePct: 0,
		history: appendRecord(cur.history,
			VersionRecord{Version: version, Action: "promote", Percent: 100, Timestamp: m.now()}),
	}
	m.state.Store(next)
	return nil
}

// Rollback restores a known-good configuration, depending on its argument:
//
//   - Rollback(candidateVersion) during a gray release: the candidate is
//     discarded immediately and 100% of users keep/return to stable.
//   - Rollback(previousStableVersion) after an instant Update: restores the
//     previous stable config atomically.
func (m *Manager) Rollback(version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.state.Load()
	switch {
	case cur.candidate != nil && cur.candidate.version == version:
		next := &managerState{
			stable:       cur.stable,
			lastStable:   cur.lastStable,
			candidate:    nil,
			candidatePct: 0,
			history: appendRecord(cur.history,
				VersionRecord{Version: version, Action: "rollback-candidate", Percent: 0, Timestamp: m.now()}),
		}
		m.state.Store(next)
		return nil
	case cur.lastStable != nil && cur.lastStable.version == version:
		next := &managerState{
			stable:       cur.lastStable,
			lastStable:   cur.stable, // allow toggling back
			candidate:    cur.candidate,
			candidatePct: cur.candidatePct,
			history: appendRecord(cur.history,
				VersionRecord{Version: version, Action: "rollback-restore", Percent: 100, Timestamp: m.now()}),
		}
		m.state.Store(next)
		return nil
	default:
		return fmt.Errorf("%w: %s (stable=%s)", ErrVersionNotFound, version, cur.stable.version)
	}
}

// Snapshot returns a traceable description of versions, rollout percentage and
// the bounded audit history.
func (m *Manager) Snapshot() StateSnapshot {
	cur := m.state.Load()
	snap := StateSnapshot{
		StableVersion:  cur.stable.version,
		RolloutPercent: cur.candidatePct,
		History:        append([]VersionRecord(nil), cur.history...),
	}
	if cur.candidate != nil {
		snap.CandidateVersion = cur.candidate.version
	}
	if cur.lastStable != nil {
		snap.PreviousVersion = cur.lastStable.version
	}
	return snap
}

// EventStats exposes backpressure counters of the event pipeline.
func (m *Manager) EventStats() QueueStats {
	if m.queue == nil {
		return QueueStats{}
	}
	return m.queue.Stats()
}

// Close stops the event pipeline (draining admitted events).
func (m *Manager) Close() error {
	if m.queue != nil {
		return m.queue.Close()
	}
	return nil
}

func (m *Manager) emit(u User, d *Decision) {
	if m.queue == nil {
		return
	}
	ts := m.now()
	for key, a := range d.Assignments {
		m.queue.Enqueue(AssignmentEvent{
			Timestamp:    ts,
			UserID:       u.ID,
			Experiment:   key,
			Variant:      a.Variant,
			InExperiment: a.InExperiment,
			Reason:       a.Reason,
			Bucket:       a.Bucket,
			ConfigFrom:   a.ConfigFrom,
		})
	}
}

func appendRecord(history []VersionRecord, rec VersionRecord) []VersionRecord {
	history = append(history, rec)
	if len(history) > MaxHistoryRecords {
		history = history[len(history)-MaxHistoryRecords:]
	}
	return history
}

// StateSnapshot describes the current versioning/rollout state.
type StateSnapshot struct {
	StableVersion    string
	CandidateVersion string
	PreviousVersion  string
	RolloutPercent   int
	History          []VersionRecord
}

// VersionRecord records one config transition for auditability.
type VersionRecord struct {
	Version   string    `json:"version"`
	Action    string    `json:"action"`
	Percent   int       `json:"percent"`
	Timestamp time.Time `json:"timestamp"`
}
