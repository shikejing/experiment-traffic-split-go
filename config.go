package split

import (
	"fmt"
	"regexp"
)

// Operator enumerates supported targeting-rule operators.
type Operator string

const (
	OpEq        Operator = "eq"
	OpNe        Operator = "ne"
	OpIn        Operator = "in"
	OpNotIn     Operator = "not_in"
	OpPrefix    Operator = "prefix"
	OpSuffix    Operator = "suffix"
	OpContains  Operator = "contains"
	OpGT        Operator = "gt"
	OpGTE       Operator = "gte"
	OpLT        Operator = "lt"
	OpLTE       Operator = "lte"
	OpRegex     Operator = "regex"
	OpExists    Operator = "exists"
	OpNotExists Operator = "not_exists"
	OpMod       Operator = "mod"
)

// Condition is one attribute predicate inside a targeting rule.
//
// Semantics:
//   - eq/ne/in/not_in compare the raw string attribute value.
//   - gt/gte/lt/lte compare numerically; when either side is not a
//     number the predicate simply does not match (never an error,
//     safe fallback to "not targeted").
//   - prefix/suffix/contains do substring matching.
//   - regex matches against a precompiled pattern in Values[0].
//   - exists/not_exists only test attribute presence.
//   - mod matches when hash-independent? No: mod computes
//     int64(attr) % ModDivisor == ModRemainder; non-numeric or invalid
//     attrs do not match.
type Condition struct {
	Attribute    string   `json:"attribute"`
	Operator     Operator `json:"operator"`
	Values       []string `json:"values,omitempty"`
	ModDivisor   int64    `json:"mod_divisor,omitempty"`
	ModRemainder int64    `json:"mod_remainder,omitempty"`
}

// TargetingRule is an OR-of-ANDs predicate: the rule matches when any
// inner group's conditions all match.
type TargetingRule struct {
	Name       string        `json:"name,omitempty"`
	Conditions [][]Condition `json:"conditions"`
}

// Variant is one arm of an experiment. Weight is a relative integer
// weight (only ratios matter, not the sum).
type Variant struct {
	Name   string `json:"name"`
	Weight int    `json:"weight"`
}

// Experiment definition.
type Experiment struct {
	ID string `json:"id"`
	// Layer optionally names a mutual-exclusion layer shared by
	// multiple experiments (alternative to explicit mutex groups).
	// Experiments with the same non-empty layer behave like a mutex
	// group and must have TrafficPercent slices summing <= 100.
	Layer string `json:"layer,omitempty"`
	// Variants of the experiment. The first variant is conventionally
	// the control arm, but any named variant is fine.
	Variants []Variant `json:"variants"`
	// TrafficPercent gates the share [0,100] of eligible users that
	// actually enter the experiment; the rest are control/not-enrolled.
	TrafficPercent int `json:"traffic_percent"`
	// Rules optionally restrict the experiment to targeted users.
	// Empty rules mean "all users".
	Rules []TargetingRule `json:"rules,omitempty"`
	// Salt, when changed, intentionally forces a full re-bucket of
	// every user (explicit, explainable migration). Bumping the salt is
	// the *only* supported way to deliberately reshuffle existing
	// users; weight-only changes never reshuffle stable users.
	Salt string `json:"salt,omitempty"`
	// Disabled removes the experiment from all allocation decisions;
	// users get the safe fallback (not enrolled).
	Disabled bool `json:"disabled,omitempty"`
}

// MutexGroup guarantees a user hits at most one member experiment.
// Member experiments occupy disjoint percentage slices of [0,100),
// ordered by experiment ID; their TrafficPercent values must sum <=100.
type MutexGroup struct {
	ID            string   `json:"id"`
	ExperimentIDs []string `json:"experiment_ids"`
}

// Config is one full, self-contained configuration snapshot.
type Config struct {
	Version     string       `json:"version"`
	Experiments []Experiment `json:"experiments"`
	Groups      []MutexGroup `json:"groups,omitempty"`
}

// Validate checks a config for internal consistency. It is called on
// every hot swap: an invalid config is rejected and never becomes
// visible to readers.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: nil config", ErrConfigInvalid)
	}
	if c.Version == "" {
		return fmt.Errorf("%w: empty version", ErrConfigInvalid)
	}
	seen := make(map[string]struct{}, len(c.Experiments))
	layers := make(map[string]int) // layer -> summed traffic percent
	for i := range c.Experiments {
		e := &c.Experiments[i]
		if e.ID == "" {
			return fmt.Errorf("%w: experiment with empty id", ErrConfigInvalid)
		}
		if _, ok := seen[e.ID]; ok {
			return fmt.Errorf("%w: duplicate experiment id %q", ErrConfigInvalid, e.ID)
		}
		seen[e.ID] = struct{}{}
		if !e.Disabled {
			if len(e.Variants) == 0 {
				return fmt.Errorf("%w: experiment %q has no variants", ErrConfigInvalid, e.ID)
			}
			var sum int
			names := make(map[string]struct{}, len(e.Variants))
			for _, v := range e.Variants {
				if v.Name == "" {
					return fmt.Errorf("%w: experiment %q has variant with empty name", ErrConfigInvalid, e.ID)
				}
				if _, ok := names[v.Name]; ok {
					return fmt.Errorf("%w: experiment %q duplicate variant %q", ErrConfigInvalid, e.ID, v.Name)
				}
				names[v.Name] = struct{}{}
				if v.Weight < 0 {
					return fmt.Errorf("%w: experiment %q variant %q negative weight", ErrConfigInvalid, e.ID, v.Name)
				}
				sum += v.Weight
			}
			if sum == 0 {
				return fmt.Errorf("%w: experiment %q total variant weight is zero", ErrConfigInvalid, e.ID)
			}
		}
		if e.TrafficPercent < 0 || e.TrafficPercent > 100 {
			return fmt.Errorf("%w: experiment %q traffic_percent %d out of [0,100]", ErrConfigInvalid, e.ID, e.TrafficPercent)
		}
		for ri := range e.Rules {
			if err := validateRule(e.ID, &e.Rules[ri]); err != nil {
				return err
			}
		}
		if e.Layer != "" {
			layers[e.Layer] += e.TrafficPercent
		}
	}
	for layer, p := range layers {
		if p > 100 {
			return fmt.Errorf("%w: layer %q traffic sums to %d (>100)", ErrConfigInvalid, layer, p)
		}
	}
	groups := make(map[string]struct{}, len(c.Groups))
	expGroup := make(map[string]string)
	for i := range c.Groups {
		g := &c.Groups[i]
		if g.ID == "" {
			return fmt.Errorf("%w: mutex group with empty id", ErrGroupInvalid)
		}
		if _, ok := groups[g.ID]; ok {
			return fmt.Errorf("%w: duplicate group id %q", ErrGroupInvalid, g.ID)
		}
		groups[g.ID] = struct{}{}
		var sum int
		for _, id := range g.ExperimentIDs {
			if _, ok := seen[id]; !ok {
				return fmt.Errorf("%w: group %q references missing experiment %q", ErrGroupInvalid, g.ID, id)
			}
			if other, dup := expGroup[id]; dup {
				return fmt.Errorf("%w: experiment %q in both groups %q and %q", ErrGroupInvalid, id, other, g.ID)
			}
			expGroup[id] = g.ID
			for _, e := range c.Experiments {
				if e.ID == id {
					sum += e.TrafficPercent
				}
			}
		}
		if sum > 100 {
			return fmt.Errorf("%w: group %q traffic sums to %d (>100)", ErrGroupInvalid, g.ID, sum)
		}
	}
	return nil
}

func validateRule(expID string, r *TargetingRule) error {
	if len(r.Conditions) == 0 {
		return fmt.Errorf("%w: experiment %q rule %q has no condition groups", ErrConfigInvalid, expID, r.Name)
	}
	for gi, group := range r.Conditions {
		if len(group) == 0 {
			return fmt.Errorf("%w: experiment %q rule %q group %d is empty", ErrConfigInvalid, expID, r.Name, gi)
		}
		for _, cond := range group {
			if cond.Attribute == "" && cond.Operator != OpExists && cond.Operator != OpNotExists {
				return fmt.Errorf("%w: experiment %q rule %q: condition has empty attribute", ErrConfigInvalid, expID, r.Name)
			}
			switch cond.Operator {
			case OpEq, OpNe, OpPrefix, OpSuffix, OpContains, OpGT, OpGTE, OpLT, OpLTE:
				if len(cond.Values) != 1 {
					return fmt.Errorf("%w: experiment %q operator %s needs exactly 1 value", ErrConfigInvalid, expID, cond.Operator)
				}
			case OpIn, OpNotIn:
				if len(cond.Values) == 0 {
					return fmt.Errorf("%w: experiment %q operator %s needs >=1 value", ErrConfigInvalid, expID, cond.Operator)
				}
			case OpRegex:
				if len(cond.Values) != 1 {
					return fmt.Errorf("%w: experiment %q regex needs exactly 1 pattern", ErrConfigInvalid, expID)
				}
				if _, err := regexp.Compile(cond.Values[0]); err != nil {
					return fmt.Errorf("%w: experiment %q invalid regex %q: %v", ErrConfigInvalid, expID, cond.Values[0], err)
				}
			case OpExists, OpNotExists:
				// no values needed
			case OpMod:
				if cond.ModDivisor <= 0 || cond.ModRemainder < 0 || cond.ModRemainder >= cond.ModDivisor {
					return fmt.Errorf("%w: experiment %q mod requires divisor>0 and 0<=remainder<divisor", ErrConfigInvalid, expID)
				}
			default:
				return fmt.Errorf("%w: experiment %q unknown operator %q", ErrConfigInvalid, expID, cond.Operator)
			}
		}
	}
	return nil
}
