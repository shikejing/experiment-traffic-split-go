package split

import (
	"fmt"
	"sync"
	"time"
)

// Rollout actions recorded for traceability.
const (
	ActionSetPercent = "set_percent"
	ActionActivate   = "activate"
	ActionRollback   = "rollback"
)

// RolloutEvent records one rollout/rollback action.
type RolloutEvent struct {
	Time    time.Time `json:"time"`
	Version string    `json:"version"`
	Percent int       `json:"percent"`
	Action  string    `json:"action"` // set_percent | activate | rollback
	Reason  string    `json:"reason"`
	Prev    string    `json:"prev,omitempty"`
}

// Roller manages per-version gray-release ramp percentages.
//
// Ramping semantics:
//   - A gray version V has a stable gate bucket b(V,userID) in [0,100).
//   - While V is at percent p, users with b < p use V; the remaining
//     users keep V's recorded fallback (previous) version. Because the
//     bucket is monotonic in p, increasing p only *adds* users to V;
//     nobody who already received V is dropped back during a ramp-up.
//   - Rollback sets p=0 and records the action; 100% of traffic resumes
//     the previous version deterministically and immediately.
type Roller struct {
	mu sync.RWMutex
	// percent is the current ramp percentage per version.
	percent map[string]int
	// prevVersion maps a gray version to its fallback version.
	prevVersion map[string]string
	audit       []RolloutEvent
}

func NewRoller() *Roller {
	return &Roller{
		percent:     make(map[string]int),
		prevVersion: make(map[string]string),
	}
}

// register records a (possibly new) version at percent 0 with its
// fallback version. Re-registering an existing version keeps its
// current percent but updates the fallback.
func (r *Roller) register(version, prev string) {
	if _, ok := r.percent[version]; !ok {
		r.percent[version] = 0
	}
	if prev != "" {
		r.prevVersion[version] = prev
	}
}

// SetPercent ramps version to percent [0,100] and records the action.
func (r *Roller) SetPercent(version string, percent int, reason string) error {
	if percent < 0 || percent > 100 {
		return fmt.Errorf("%w: %d", ErrPercentInvalid, percent)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.percent[version] = percent
	r.audit = append(r.audit, RolloutEvent{
		Time: time.Now().UTC(), Version: version, Percent: percent,
		Action: ActionSetPercent, Reason: reason, Prev: r.prevVersion[version],
	})
	return nil
}

// activate marks version as serving 100% of traffic and records it.
func (r *Roller) activate(version, reason string) {
	r.percent[version] = 100
	r.audit = append(r.audit, RolloutEvent{
		Time: time.Now().UTC(), Version: version, Percent: 100,
		Action: ActionActivate, Reason: reason, Prev: r.prevVersion[version],
	})
}

// SetPrev records the fallback version for a new (gray) version.
func (r *Roller) SetPrev(version, prev string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prevVersion[version] = prev
}

// ResolveVersion picks which config version serves userID given the
// ramp state for newVersion. It returns newVersion when the user is in
// the ramped slice, otherwise the registered fallback version.
func (r *Roller) ResolveVersion(newVersion, userID string) string {
	r.mu.RLock()
	p := r.percent[newVersion]
	prev := r.prevVersion[newVersion]
	r.mu.RUnlock()
	if p <= 0 {
		return prev
	}
	if p >= 100 {
		return newVersion
	}
	bucket := hash100("rollout:" + newVersion + "|" + userID)
	if int(bucket) < p {
		return newVersion
	}
	return prev
}

// PercentOf returns the current ramp percent of version (0 if unknown).
func (r *Roller) PercentOf(version string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.percent[version]
}

// prevOf returns the registered fallback of version.
func (r *Roller) prevOf(version string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.prevVersion[version]
}

// hasVersion reports whether version is registered.
func (r *Roller) hasVersion(version string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.percent[version]
	return ok
}

// Rollback sets version to 0% so all traffic returns to its recorded
// fallback immediately, and appends an audit entry. It returns the
// restored fallback version.
func (r *Roller) Rollback(version, reason string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.prevVersion[version]
	if prev == "" {
		return "", fmt.Errorf("%w: no fallback registered for %q", ErrVersionMissing, version)
	}
	r.percent[version] = 0
	r.audit = append(r.audit, RolloutEvent{
		Time: time.Now().UTC(), Version: version, Percent: 0,
		Action: ActionRollback, Reason: reason, Prev: prev,
	})
	return prev, nil
}

// Audit returns a defensive copy of the full rollout/rollback history.
func (r *Roller) Audit() []RolloutEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RolloutEvent, len(r.audit))
	copy(out, r.audit)
	return out
}
