package experiment

import (
	"fmt"
	"strings"
)

// Scale is the bucketing resolution: hashes are mapped to [0, Scale).
const Scale uint32 = 10000

// Config is one versioned, complete experiment configuration snapshot.
type Config struct {
	Version     string             `json:"version" yaml:"version"`
	Description string             `json:"description,omitempty" yaml:"description,omitempty"`
	Experiments []ExperimentConfig `json:"experiments" yaml:"experiments"`
	Layers      []LayerConfig      `json:"layers,omitempty" yaml:"layers,omitempty"`
}

// LayerConfig declares a mutually exclusive layer.
type LayerConfig struct {
	Key         string   `json:"key" yaml:"key"`
	Traffic     int      `json:"traffic,omitempty" yaml:"traffic,omitempty"` // 0..100 percent of users that may enter the layer; <=0 or 100 means all
	Experiments []string `json:"experiments" yaml:"experiments"`             // experiment keys participating in this layer
}

// ExperimentConfig declares one experiment.
type ExperimentConfig struct {
	Key          string          `json:"key" yaml:"key"`
	Enabled      bool            `json:"enabled" yaml:"enabled"`
	Layer        string          `json:"layer,omitempty" yaml:"layer,omitempty"`
	Variants     []VariantConfig `json:"variants" yaml:"variants"`
	Rules        *RuleConfig     `json:"rules,omitempty" yaml:"rules,omitempty"`
	Fallback     string          `json:"fallback,omitempty" yaml:"fallback,omitempty"` // variant served to non-bucketed users when non-empty
	ReBucketSalt string          `json:"rebucket_salt,omitempty" yaml:"rebucket_salt,omitempty"`
}

// VariantConfig declares one variant and its relative weight.
type VariantConfig struct {
	Key    string `json:"key" yaml:"key"`
	Weight int    `json:"weight" yaml:"weight"`
}

// RuleConfig is the targeting rule tree. When nil the experiment targets all.
type RuleConfig struct {
	Op    string           `json:"op,omitempty" yaml:"op,omitempty"` // "and" (default) or "or"
	All   []RuleConfig     `json:"all,omitempty" yaml:"all,omitempty"`
	Any   []RuleConfig     `json:"any,omitempty" yaml:"any,omitempty"`
	Where *ConditionConfig `json:"where,omitempty" yaml:"where,omitempty"`
}

// ConditionConfig is one leaf condition.
type ConditionConfig struct {
	Attribute string  `json:"attribute" yaml:"attribute"`
	Operator  string  `json:"operator" yaml:"operator"` // eq, ne, in, not_in, gt, gte, lt, lte, exists, not_exists, prefix
	Values    []Value `json:"values,omitempty" yaml:"values,omitempty"`
}

// Validate checks structural validity of a complete configuration snapshot.
// A config that fails validation must never become active: the manager keeps
// serving the previous good snapshot.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("config version must not be empty")
	}
	if len(c.Experiments) == 0 {
		return fmt.Errorf("config %s: at least one experiment is required", c.Version)
	}

	expByKey := make(map[string]*ExperimentConfig, len(c.Experiments))
	for i := range c.Experiments {
		e := &c.Experiments[i]
		if strings.TrimSpace(e.Key) == "" {
			return fmt.Errorf("config %s: experiment key must not be empty", c.Version)
		}
		if _, dup := expByKey[e.Key]; dup {
			return fmt.Errorf("config %s: duplicate experiment key %q", c.Version, e.Key)
		}
		expByKey[e.Key] = e

		ranges, _, err := buildRanges(e.Variants)
		if err != nil {
			return fmt.Errorf("config %s: experiment %q: %w", c.Version, e.Key, err)
		}
		if e.Fallback != "" {
			if pickVariant(ranges, 0) == "" {
				return fmt.Errorf("config %s: experiment %q: invalid variant table", c.Version, e.Key)
			}
			found := false
			for _, r := range ranges {
				if r.Key == e.Fallback {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("config %s: experiment %q: fallback %q is not a declared variant", c.Version, e.Key, e.Fallback)
			}
		}
		if _, err := compileRule(e.Rules); err != nil {
			return fmt.Errorf("config %s: experiment %q: %w", c.Version, e.Key, err)
		}
	}

	seenLayerExp := make(map[string]string) // experiment key -> layer key
	layerByKey := make(map[string]*LayerConfig, len(c.Layers))
	for i := range c.Layers {
		l := &c.Layers[i]
		if strings.TrimSpace(l.Key) == "" {
			return fmt.Errorf("config %s: layer key must not be empty", c.Version)
		}
		if _, dup := layerByKey[l.Key]; dup {
			return fmt.Errorf("config %s: duplicate layer key %q", c.Version, l.Key)
		}
		layerByKey[l.Key] = l
		if l.Traffic < 0 || l.Traffic > 100 {
			return fmt.Errorf("config %s: layer %q: traffic must be in [0,100]", c.Version, l.Key)
		}
		if len(l.Experiments) == 0 {
			return fmt.Errorf("config %s: layer %q: must contain at least one experiment", c.Version, l.Key)
		}
		for _, ek := range l.Experiments {
			e, ok := expByKey[ek]
			if !ok {
				return fmt.Errorf("config %s: layer %q: unknown experiment %q", c.Version, l.Key, ek)
			}
			if e.Layer != "" && e.Layer != l.Key {
				return fmt.Errorf("config %s: experiment %q declares layer %q but is listed in layer %q",
					c.Version, ek, e.Layer, l.Key)
			}
			if other, dup := seenLayerExp[ek]; dup {
				return fmt.Errorf("config %s: experiment %q is in two layers (%q, %q)", c.Version, ek, other, l.Key)
			}
			seenLayerExp[ek] = l.Key
		}
	}

	// Experiments naming a layer must be a member of that layer.
	for _, e := range c.Experiments {
		if e.Layer == "" {
			continue
		}
		if _, ok := layerByKey[e.Layer]; !ok {
			return fmt.Errorf("config %s: experiment %q references unknown layer %q", c.Version, e.Key, e.Layer)
		}
		if seenLayerExp[e.Key] != e.Layer {
			return fmt.Errorf("config %s: experiment %q declares layer %q but is not in its member list",
				c.Version, e.Key, e.Layer)
		}
	}
	return nil
}
