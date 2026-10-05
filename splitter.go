package split

import (
	"context"
	"sync"
)

// Options configures a Splitter manager.
type Options struct {
	// Recorder optionally records assignment events asynchronously.
	Recorder *EventRecorder
}

// Splitter is the reusable component entry point. It owns the config
// store, the gray-release roller, the allocator and (optionally) an
// async event recorder.
//
// Concurrency model:
//   - The read hot path (Allocate) takes an RLock only to read the
//     active-version pointer; the actual allocation is lock-free on
//     immutable compiled configs.
//   - Writes (PutConfig/Promote/Ramp/Rollback/HotReplace) are
//     serialized by the write lock. Readers always observe a complete
//     config pair (active + gray), never a half-updated one.
type Splitter struct {
	mu       sync.RWMutex
	store    *Store
	roller   *Roller
	rec      *EventRecorder
	versions map[string]*compiledConfig
	active   string // fully-serving version
	gray     string // candidate version being ramped ("" if none)
}

// New builds a Splitter from an initial validated config. The initial
// config is registered and immediately active at 100%.
func New(initial *Config, opts Options) (*Splitter, error) {
	if initial == nil {
		return nil, ErrConfigInvalid
	}
	cc, err := compile(initial)
	if err != nil {
		return nil, err
	}
	store, err := NewStore(initial)
	if err != nil {
		return nil, err
	}
	s := &Splitter{
		store:    store,
		roller:   NewRoller(),
		rec:      opts.Recorder,
		versions: map[string]*compiledConfig{cc.version: cc},
		active:   cc.version,
	}
	s.roller.register(cc.version, "")
	s.roller.activate(cc.version, "initial")
	return s, nil
}

// Allocate assigns userID within experimentID, using the gray-release
// state to pick the serving config version for this specific user.
//
// It never fails because of missing/abnormal attributes: such users
// deterministically get the safe fallback (not enrolled). It returns
// ErrExperimentGone only when the experiment is absent from the
// resolved config.
func (s *Splitter) Allocate(ctx context.Context, experimentID, userID string, attrs Attributes) (Allocation, error) {
	cc, version := s.resolveConfig(userID)
	if cc == nil {
		return Allocation{ExperimentID: experimentID, Reason: "no_config"}, ErrConfigInvalid
	}
	al, err := newAllocator(cc).Allocate(ctx, experimentID, userID, attrs)
	if s.rec != nil {
		s.rec.Record(AssignmentEvent{
			ExperimentID:  experimentID,
			Variant:       al.Variant,
			InExperiment:  al.InExperiment,
			UserID:        userID,
			ConfigVersion: version,
			Reason:        al.Reason,
		})
	}
	return al, err
}

// AllocateAll returns the user's allocation against every experiment in
// deterministic (sorted ID) order. Within a mutex group/layer at most
// one entry has InExperiment==true. All entries share the same
// resolved config version, which is also returned.
func (s *Splitter) AllocateAll(ctx context.Context, userID string, attrs Attributes) (string, []Allocation, error) {
	cc, version := s.resolveConfig(userID)
	if cc == nil {
		return "", nil, ErrConfigInvalid
	}
	alloc := newAllocator(cc)
	out := make([]Allocation, 0, len(cc.order))
	for _, id := range cc.order {
		al, err := alloc.Allocate(ctx, id, userID, attrs)
		if err != nil {
			return version, out, err
		}
		out = append(out, al)
		if s.rec != nil {
			s.rec.Record(AssignmentEvent{
				ExperimentID:  id,
				Variant:       al.Variant,
				InExperiment:  al.InExperiment,
				UserID:        userID,
				ConfigVersion: version,
				Reason:        al.Reason,
			})
		}
	}
	return version, out, nil
}

// resolveConfig selects the compiled config that serves userID:// the active one, unless a registered gray version's ramp slice
// contains the user.
func (s *Splitter) resolveConfig(userID string) (*compiledConfig, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	active := s.active
	gray := ""
	// Exactly one gray version is supported at a time: a registered
	// version with percent in (0,100) (or at 0 pending ramp). Find it
	// deterministically via audit-independent scan; keep the newest by
	// roller state. Simpler: track the gray explicitly via field.
	if s.gray != "" {
		gray = s.gray
	}
	if gray == "" {
		return s.versions[active], active
	}
	chosen := s.roller.ResolveVersion(gray, userID)
	cc, ok := s.versions[chosen]
	if !ok {
		return s.versions[active], active
	}
	return cc, chosen
}

// PutConfig registers a new validated config version without serving
// traffic. It becomes the current "gray" candidate with 0% ramp; its
// fallback is the currently active version.
func (s *Splitter) PutConfig(cfg *Config) error {
	cc, err := compile(cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.versions[cc.version]; exists {
		return ErrConfigInvalid // re-registering a version is forbidden
	}
	s.versions[cc.version] = cc
	s.roller.register(cc.version, s.active)
	s.gray = cc.version
	return nil
}

// Ramp sets the gray-release percentage of a registered candidate.
// percent==0 keeps everyone on the fallback; 100 serves only the
// candidate. Ramp changes are audited.
func (s *Splitter) Ramp(cfgVersion string, percent int, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.versions[cfgVersion]; !ok {
		return ErrVersionMissing
	}
	if err := s.roller.SetPercent(cfgVersion, percent, reason); err != nil {
		return err
	}
	if cfgVersion != s.gray {
		s.gray = cfgVersion
	}
	return nil
}

// Promote switches 100% of traffic to cfgVersion atomically. The
// store's atomic pointer swap means every request sees either the old
// or the new complete config. The previous active version is retained
// as rollback target.
func (s *Splitter) Promote(cfgVersion, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cc, ok := s.versions[cfgVersion]
	if !ok {
		return ErrVersionMissing
	}
	prev := s.active
	if err := s.store.TryReplace(cc.raw, nil); err != nil {
		return err
	}
	s.active = cfgVersion
	s.roller.SetPrev(cfgVersion, prev)
	s.roller.activate(cfgVersion, reason)
	if s.gray == cfgVersion {
		s.gray = ""
	}
	return nil
}

// RollbackVersion immediately moves 100% of traffic back to the
// affected version's registered fallback (ramp percent -> 0). If the
// version was already fully promoted, the store is atomically swapped
// back to the previous config. Every step is audited.
func (s *Splitter) RollbackVersion(cfgVersion, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.versions[cfgVersion]; !ok {
		return ErrVersionMissing
	}
	restored, err := s.roller.Rollback(cfgVersion, reason)
	if err != nil {
		return err
	}
	if cfgVersion == s.active {
		cc, ok := s.versions[restored]
		if !ok {
			return ErrVersionMissing
		}
		if err := s.store.TryReplace(cc.raw, nil); err != nil {
			return err
		}
		s.active = restored
	}
	if s.gray == cfgVersion {
		s.gray = ""
	}
	return nil
}

// HotReplace performs a simple atomic hot update without gray release:
// validate, then single-pointer swap. Any validation failure leaves
// the previous config serving 100% of traffic. On success the new
// config is registered and immediately active.
func (s *Splitter) HotReplace(cfg *Config, reason string) error {
	cc, err := compile(cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.active
	if err := s.store.TryReplace(cc.raw, nil); err != nil {
		return err
	}
	if _, ok := s.versions[cc.version]; !ok {
		s.versions[cc.version] = cc
	}
	s.roller.register(cc.version, prev)
	s.roller.activate(cc.version, reason)
	s.active = cc.version
	s.gray = ""
	return nil
}

// CurrentVersion reports the fully-active config version.
func (s *Splitter) CurrentVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// GrayPercent reports the current ramp percentage of the gray
// candidate ("" when none is ramping).
func (s *Splitter) GrayState() (version string, percent int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.gray == "" {
		return "", 0
	}
	return s.gray, s.roller.PercentOf(s.gray)
}

// RolloutAudit returns a copy of the gray-release/rollback trace.
func (s *Splitter) RolloutAudit() []RolloutEvent { return s.roller.Audit() }

// ForgetVersion drops a registered non-active version and frees its
// memory. Use it to bound retained history; the active and current gray
// versions cannot be forgotten.
func (s *Splitter) ForgetVersion(version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version == s.active || version == s.gray {
		return ErrConfigInvalid
	}
	if _, ok := s.versions[version]; !ok {
		return ErrVersionMissing
	}
	delete(s.versions, version)
	return nil
}

// Close flushes the event recorder if one is configured.
func (s *Splitter) Close(ctx context.Context) error {
	if s.rec != nil {
		return s.rec.Flush(ctx)
	}
	return nil
}
