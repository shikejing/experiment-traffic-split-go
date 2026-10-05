package experiment

import "fmt"

// Additional reason code for layer mutual exclusion.
const (
	// ReasonLayerOther means the user deterministically hashed into another
	// member experiment's slot within the same layer.
	ReasonLayerOther = "layer_slot_assigned_to_other_experiment"
)

// engine is an immutable, compiled configuration snapshot. All fields are
// read-only after construction, so concurrent Evaluate calls need no locks.
type engine struct {
	version     string
	description string
	experiments map[string]*compiledExperiment
	layers      map[string]*compiledLayer
	order       []string // experiments in config declaration order
}

type compiledExperiment struct {
	cfg    ExperimentConfig
	ranges []VariantRange
	total  int
	rule   rule // nil-safe; matchAll when no rules
	layer  *compiledLayer
}

type compiledLayer struct {
	key          string
	trafficPct   uint32 // 0 means all users may enter
	memberRanges []VariantRange
	members      map[string]bool
}

// compileEngine builds an immutable engine from a validated config.
func compileEngine(cfg *Config) (*engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	e := &engine{
		version:     cfg.Version,
		description: cfg.Description,
		experiments: make(map[string]*compiledExperiment, len(cfg.Experiments)),
		layers:      make(map[string]*compiledLayer, len(cfg.Layers)),
		order:       make([]string, 0, len(cfg.Experiments)),
	}
	for i := range cfg.Layers {
		lc := &cfg.Layers[i]
		vars := make([]VariantConfig, 0, len(lc.Experiments))
		for _, mk := range lc.Experiments {
			vars = append(vars, VariantConfig{Key: mk, Weight: 1})
		}
		mr, _, err := buildRanges(vars) // equal-weight slots; deterministic
		if err != nil {
			return nil, fmt.Errorf("layer %q: %w", lc.Key, err)
		}
		mset := make(map[string]bool, len(lc.Experiments))
		for _, mk := range lc.Experiments {
			mset[mk] = true
		}
		e.layers[lc.Key] = &compiledLayer{
			key:          lc.Key,
			trafficPct:   uint32(lc.Traffic),
			memberRanges: mr,
			members:      mset,
		}
	}
	for i := range cfg.Experiments {
		ec := &cfg.Experiments[i]
		ranges, total, err := buildRanges(ec.Variants)
		if err != nil {
			return nil, fmt.Errorf("experiment %q: %w", ec.Key, err)
		}
		r, err := compileRule(ec.Rules)
		if err != nil {
			return nil, fmt.Errorf("experiment %q: %w", ec.Key, err)
		}
		ce := &compiledExperiment{cfg: *ec, ranges: ranges, total: total, rule: r}
		if ec.Layer != "" {
			ce.layer = e.layers[ec.Layer]
		}
		e.experiments[ec.Key] = ce
		e.order = append(e.order, ec.Key)
	}
	return e, nil
}

// evaluate computes assignments for every experiment for a user against this
// immutable snapshot. It never returns an error: missing attributes or odd
// values resolve to deterministic, explained non-hits.
func (e *engine) evaluate(u User) *Decision {
	d := &Decision{
		Assignments: make(map[string]*Assignment, len(e.order)),
		ConfigFrom:  e.version,
	}

	// Pre-compute, per layer, the single member experiment slot this user
	// belongs to (or the layer-wide exclusion reason).
	type layerGate struct {
		slotted string // member key that owns this user's slot, "" if excluded
		reason  string // exclusion reason for all members
	}
	gates := make(map[string]*layerGate, len(e.layers))
	for key, l := range e.layers {
		g := &layerGate{}
		if l.trafficPct > 0 {
			p := hashPercent("layer-traffic:" + key + ":" + u.ID)
			if p >= l.trafficPct {
				g.reason = ReasonLayerExcluded
				gates[key] = g
				continue
			}
		}
		slotBucket := hashUser("", "layer-slot:"+key, u.ID)
		g.slotted = pickVariant(l.memberRanges, slotBucket)
		gates[key] = g
	}

	for _, key := range e.order {
		ce := e.experiments[key]
		a := &Assignment{Experiment: key, ConfigFrom: e.version}

		if ce.layer != nil {
			g := gates[ce.layer.key]
			switch {
			case g.reason != "":
				a.Reason = g.reason
				a.Variant = ce.cfg.Fallback // may be ""
				d.Assignments[key] = a
				continue
			case g.slotted != key:
				// Mutual exclusion: the user belongs to another member's slot.
				a.Reason = ReasonLayerOther
				a.Variant = ce.cfg.Fallback
				d.Assignments[key] = a
				continue
			}
		}

		e.evaluateOne(ce, u, a)
		d.Assignments[key] = a
	}
	return d
}

// evaluateOne fills an assignment for a single non-excluded experiment.
func (e *engine) evaluateOne(ce *compiledExperiment, u User, a *Assignment) {
	if !ce.cfg.Enabled {
		a.Reason = ReasonExperimentOff
		a.Variant = ce.cfg.Fallback
		return
	}
	if ok, why := ce.rule.match(u.Attributes); !ok {
		a.Reason = ReasonTargetingMismatch + ": " + why
		a.Variant = ce.cfg.Fallback
		return
	}

	bucket := hashUser(ce.cfg.ReBucketSalt, "experiment:"+ce.cfg.Key, u.ID)
	a.Bucket = bucket
	variant := pickVariant(ce.ranges, bucket)
	if variant == "" {
		// Defensive only: buildRanges guarantees full coverage.
		a.Reason = ReasonNoVariants
		a.Variant = ce.cfg.Fallback
		return
	}
	a.InExperiment = true
	a.Variant = variant
	a.Reason = ReasonAssigned
}
