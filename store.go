package split

import "sync/atomic"

// Store holds the currently active immutable compiled config and
// supports atomic hot-swap with automatic rollback.
//
// Concurrency contract:
//   - Readers (Current) are lock-free: they load one atomic pointer to
//     a fully-built snapshot, so they always observe either the old or
//     the new complete config — never a half-updated one.
//   - During a swap all in-flight requests continue against the old
//     snapshot, so throughput does not dip: the swap is a single
//     atomic pointer store.
//   - A failed validation or a failing post-swap hook rolls the pointer
//     back to the previous snapshot.
type Store struct {
	current atomic.Pointer[storeSnapshot]
}

type storeSnapshot struct {
	compiled *compiledConfig
	raw      *Config
}

// NewStore validates initial before serving it. An empty/nil initial
// config is allowed (an empty store serving only safe fallbacks) and
// must later be populated via Replace.
func NewStore(initial *Config) (*Store, error) {
	s := &Store{}
	if initial != nil {
		cc, err := compile(initial)
		if err != nil {
			return nil, err
		}
		s.current.Store(&storeSnapshot{compiled: cc, raw: cloneConfig(initial)})
	}
	return s, nil
}

// Current returns the active compiled snapshot, or nil when the store
// has no config yet. Callers must not mutate it.
func (s *Store) Current() *compiledConfig {
	snap := s.current.Load()
	if snap == nil {
		return nil
	}
	return snap.compiled
}

// Raw returns a deep copy of the active raw config (safe for callers
// to mutate), or nil when empty.
func (s *Store) Raw() *Config {
	snap := s.current.Load()
	if snap == nil {
		return nil
	}
	return cloneConfig(snap.raw)
}

// Replace validates and atomically activates cfg. On any failure the
// previous config stays active and the error is returned.
func (s *Store) Replace(cfg *Config) error {
	return s.TryReplace(cfg, nil)
}

// TryReplace compiles cfg (full validation, no traffic impact), then
// atomically swaps it in and runs fn against the now-active candidate.
// If fn fails, the previous snapshot is restored atomically and the
// error is returned: readers either saw the old config or the new
// complete config during the window, never a partial state.
func (s *Store) TryReplace(cfg *Config, fn func(*compiledConfig) error) error {
	if cfg == nil {
		return ErrConfigInvalid
	}
	cc, err := compile(cfg)
	if err != nil {
		return err
	}
	prev := s.current.Load()
	next := &storeSnapshot{compiled: cc, raw: cloneConfig(cfg)}
	s.current.Store(next)
	if fn != nil {
		if hookErr := fn(cc); hookErr != nil {
			// Rollback: restore whatever was active before.
			s.current.Store(prev)
			return hookErr
		}
	}
	return nil
}

// cloneConfig returns a deep enough copy that callers mutating the
// returned value cannot corrupt an active snapshot.
func cloneConfig(c *Config) *Config {
	if c == nil {
		return nil
	}
	cp := &Config{Version: c.Version}
	cp.Experiments = make([]Experiment, len(c.Experiments))
	for i := range c.Experiments {
		e := c.Experiments[i]
		e.Variants = append([]Variant(nil), e.Variants...)
		if e.Rules != nil {
			e.Rules = append([]TargetingRule(nil), e.Rules...)
			for ri := range e.Rules {
				e.Rules[ri].Conditions = append([][]Condition(nil), e.Rules[ri].Conditions...)
				for ci := range e.Rules[ri].Conditions {
					e.Rules[ri].Conditions[ci] = append([]Condition(nil), e.Rules[ri].Conditions[ci]...)
				}
			}
		}
		cp.Experiments[i] = e
	}
	cp.Groups = make([]MutexGroup, len(c.Groups))
	for i := range c.Groups {
		cp.Groups[i].ID = c.Groups[i].ID
		cp.Groups[i].ExperimentIDs = append([]string(nil), c.Groups[i].ExperimentIDs...)
	}
	return cp
}
