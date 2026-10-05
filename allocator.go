package split

import (
	"context"
	"sort"
)

// Allocation is the explainable result of assigning one user.
type Allocation struct {
	ExperimentID string
	Variant      string
	// InExperiment is false when the user does not enter the
	// experiment: not targeted, outside the traffic slice, or disabled.
	InExperiment bool
	// GateBucket is the user's stable [0,100) gate bucket for this
	// experiment, exposed for debugging/traceability.
	GateBucket uint32
	// Reason is a machine-readable explanation of the decision.
	Reason string
}

// compiledExperiment is the immutable, lookup-optimized experiment form.
type compiledExperiment struct {
	*Experiment
	rules []compiledRule
}

// compiledConfig is the immutable, lookup-optimized form of Config.
// Compiled configs are shared by pointer across goroutines and never
// mutated after construction.
type compiledConfig struct {
	version     string
	raw         *Config
	experiments map[string]*compiledExperiment
	groups      map[string]*MutexGroup
	expGroup    map[string]string
	order       []string // experiment IDs sorted, deterministic iteration
	// slice[expID] gives the [start,end) percentage slice inside a
	// mutex group / layer, or [0,traffic_percent) for standalone
	// experiments.
	slice map[string][2]int
	// gateScope[expID] names the shared gate domain. Members of one
	// mutex group / layer share the SAME gate bucket, so disjoint
	// slices truly imply mutual exclusivity. Standalone experiments use
	// their own (experiment,salt) domain.
	gateScope map[string]string
}

func compile(c *Config) (*compiledConfig, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	cc := &compiledConfig{
		version:     c.Version,
		raw:         c,
		experiments: make(map[string]*compiledExperiment, len(c.Experiments)),
		groups:      make(map[string]*MutexGroup, len(c.Groups)),
		expGroup:    make(map[string]string),
		slice:       make(map[string][2]int),
		gateScope:   make(map[string]string),
	}
	for i := range c.Experiments {
		e := &c.Experiments[i]
		rules, err := compileRules(e.ID, e.Rules)
		if err != nil {
			return nil, err
		}
		cc.experiments[e.ID] = &compiledExperiment{Experiment: e, rules: rules}
		cc.order = append(cc.order, e.ID)
	}
	sort.Strings(cc.order)

	// Explicit mutex groups: disjoint slices, members sorted by ID.
	for i := range c.Groups {
		g := &c.Groups[i]
		cc.groups[g.ID] = g
		ids := append([]string(nil), g.ExperimentIDs...)
		sort.Strings(ids)
		start := 0
		for _, id := range ids {
			cc.expGroup[id] = g.ID
			cc.gateScope[id] = "group:" + g.ID
			pct := cc.experiments[id].TrafficPercent
			cc.slice[id] = [2]int{start, start + pct}
			start += pct
		}
	}

	// Implicit layers: same disjoint-slice semantics by experiment ID.
	layerMembers := map[string][]string{}
	for _, id := range cc.order {
		if l := cc.experiments[id].Layer; l != "" {
			if _, inGroup := cc.expGroup[id]; !inGroup {
				layerMembers[l] = append(layerMembers[l], id)
			}
		}
	}
	for layer, ids := range layerMembers {
		sort.Strings(ids) // order is already sorted; keep explicit
		start := 0
		for _, id := range ids {
			cc.gateScope[id] = "layer:" + layer
			pct := cc.experiments[id].TrafficPercent
			cc.slice[id] = [2]int{start, start + pct}
			start += pct
		}
	}

	// Standalone experiments: own [0,traffic_percent) gate.
	for _, id := range cc.order {
		if _, ok := cc.slice[id]; !ok {
			pct := cc.experiments[id].TrafficPercent
			cc.slice[id] = [2]int{0, pct}
			cc.gateScope[id] = "exp:" + id
		}
	}
	return cc, nil
}

// Allocator performs pure allocations against one immutable compiled
// config. It is safe for concurrent use and allocates no per-request
// memory beyond the returned value.
type Allocator struct {
	cfg *compiledConfig
}

func newAllocator(cfg *compiledConfig) *Allocator {
	return &Allocator{cfg: cfg}
}

func (a *Allocator) Version() string { return a.cfg.version }

// gateKey/variantKeyPrefix put the user id LAST in the hashed string
// so FNV mixes it into the low bits consumed by hash100/unitFloat.
//
// Gate and variant hashes use INDEPENDENT domains: the traffic gate
// bucket never correlates with which variant a user receives.
func gateKey(scope, userID string) string {
	return "gate|" + scope + "|" + userID
}

func variantKeyPrefix(expID, salt string) string {
	return "var|" + expID + "|" + salt + "|"
}

// allocateFor assigns the user inside a single experiment whose
// enrollment slice is [start,end).
//
// Stability contract:
//   - The gate bucket is a pure function of (experimentID, salt, userID)
//     => stable for the user's lifetime unless the salt is deliberately
//     bumped.
//   - The variant is chosen by weighted rendezvous hashing => weight
//     adjustments move only the minimal set of users; untouched users
//     never drift.
//   - A salt change is an explicit, explainable full re-bucket.
func (a *Allocator) allocateFor(exp *compiledExperiment, userID, gateScope string, start, end int) Allocation {
	al := Allocation{ExperimentID: exp.ID}
	bucket := hash100(gateKey(gateScope, userID))
	al.GateBucket = bucket
	if int(bucket) < start || int(bucket) >= end {
		al.Reason = "not_enrolled_traffic_slice"
		return al
	}
	name := weightedPick(variantKeyPrefix(exp.ID, exp.Salt), userID, exp.Variants)
	if name == "" {
		al.Reason = "not_enrolled_no_positive_weight"
		return al
	}
	al.InExperiment = true
	al.Variant = name
	al.Reason = "enrolled"
	return al
}

// Allocate finds the assignment of a user within one experiment,
// enforcing targeting and the mutex-group/layer slice semantics.
//
// Missing or abnormal attributes never cause an error: they result in
// the safe fallback (not enrolled). Only an absent experiment ID
// returns ErrExperimentGone.
func (a *Allocator) Allocate(_ context.Context, experimentID, userID string, attrs Attributes) (Allocation, error) {
	exp, ok := a.cfg.experiments[experimentID]
	if !ok {
		return Allocation{ExperimentID: experimentID, Reason: "experiment_not_found"}, ErrExperimentGone
	}
	if exp.Disabled {
		return Allocation{ExperimentID: experimentID, Reason: "experiment_disabled"}, nil
	}
	if rr := matchCompiledRules(exp.rules, attrs); !rr.Matched {
		return Allocation{ExperimentID: experimentID, Reason: "not_targeted:" + rr.Reason}, nil
	}
	sl := a.cfg.slice[experimentID]
	return a.allocateFor(exp, userID, a.cfg.gateScope[experimentID], sl[0], sl[1]), nil
}
