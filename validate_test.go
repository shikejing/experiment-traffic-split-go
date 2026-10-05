package split

import (
	"strings"
	"testing"
)

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
	}{
		{"nil", nil},
		{"empty version", &Config{Experiments: []Experiment{{ID: "e", Variants: []Variant{{Name: "a", Weight: 1}}, TrafficPercent: 100}}}},
		{"dup experiment", &Config{Version: "v", Experiments: []Experiment{
			{ID: "e", Variants: []Variant{{Name: "a", Weight: 1}}, TrafficPercent: 100},
			{ID: "e", Variants: []Variant{{Name: "a", Weight: 1}}, TrafficPercent: 100},
		}}},
		{"no variants", &Config{Version: "v", Experiments: []Experiment{{ID: "e", TrafficPercent: 100}}}},
		{"zero weight", &Config{Version: "v", Experiments: []Experiment{{ID: "e", Variants: []Variant{{Name: "a", Weight: 0}}, TrafficPercent: 100}}}},
		{"bad traffic", &Config{Version: "v", Experiments: []Experiment{{ID: "e", Variants: []Variant{{Name: "a", Weight: 1}}, TrafficPercent: 101}}}},
		{"bad op", &Config{Version: "v", Experiments: []Experiment{{
			ID: "e", TrafficPercent: 100, Variants: []Variant{{Name: "a", Weight: 1}},
			Rules: []TargetingRule{{Conditions: [][]Condition{{{Attribute: "a", Operator: "weird", Values: []string{"x"}}}}}},
		}}}},
		{"bad regex", &Config{Version: "v", Experiments: []Experiment{{
			ID: "e", TrafficPercent: 100, Variants: []Variant{{Name: "a", Weight: 1}},
			Rules: []TargetingRule{{Conditions: [][]Condition{{{Attribute: "a", Operator: OpRegex, Values: []string{"["}}}}}},
		}}}},
		{"mod bad", &Config{Version: "v", Experiments: []Experiment{{
			ID: "e", TrafficPercent: 100, Variants: []Variant{{Name: "a", Weight: 1}},
			Rules: []TargetingRule{{Conditions: [][]Condition{{{Attribute: "a", Operator: OpMod, ModDivisor: 0}}}}},
		}}}},
		{"group missing exp", &Config{Version: "v", Experiments: []Experiment{{ID: "e", Variants: []Variant{{Name: "a", Weight: 1}}, TrafficPercent: 100}},
			Groups: []MutexGroup{{ID: "g", ExperimentIDs: []string{"ghost"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateOK(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRollerPercentBounds(t *testing.T) {
	r := NewRoller()
	r.register("v2", "v1")
	if err := r.SetPercent("v2", 101, ""); err == nil {
		t.Fatal("percent >100 must fail")
	}
	if err := r.SetPercent("v2", -1, ""); err == nil {
		t.Fatal("negative percent must fail")
	}
	if r.ResolveVersion("unknown", "u") != "" {
		t.Fatal("unregistered version resolves to empty fallback")
	}
	r.SetPercent("v2", 0, "x")
	if got := r.ResolveVersion("v2", "u"); got != "v1" {
		t.Fatalf("0%% resolves to %q, want v1", got)
	}
	if err := r.SetPercent("v2", 100, "full"); err != nil {
		t.Fatal(err)
	}
	if got := r.ResolveVersion("v2", "u"); got != "v2" {
		t.Fatalf("100%% resolves to %q, want v2", got)
	}
	// Ramp is monotonic: at 30% the set of new-version users is a
	// prefix of the 60% set.
	r.SetPercent("v2", 30, "")
	in30 := map[string]bool{}
	for i := 0; i < 2000; i++ {
		if r.ResolveVersion("v2", userN(i)) == "v2" {
			in30[userN(i)] = true
		}
	}
	r.SetPercent("v2", 60, "")
	for u := range in30 {
		if r.ResolveVersion("v2", u) != "v2" {
			t.Fatalf("user %s dropped from new version during ramp-up", u)
		}
	}
	// Audit is a copy (mutating it must not corrupt internal state).
	audit := r.Audit()
	audit[0] = RolloutEvent{}
	if r.Audit()[0].Action == "" {
		t.Fatal("Audit must return defensive copies")
	}
}

func TestRampUnknownVersionFails(t *testing.T) {
	s, _ := New(testConfig(), Options{})
	if err := s.Ramp("nope", 10, ""); err == nil || !strings.Contains(err.Error(), "version not found") {
		t.Fatalf("want ErrVersionMissing, got %v", err)
	}
	if err := s.RollbackVersion("nope", ""); err == nil {
		t.Fatal("rollback unknown version must fail")
	}
}

func TestPutDuplicateVersionFails(t *testing.T) {
	s, _ := New(testConfig(), Options{})
	if err := s.PutConfig(testConfig()); err == nil {
		t.Fatal("re-registering active version must fail")
	}
}
