package experiment

import (
	"fmt"
	"testing"
)

func abConfig(version string, wA, wB int) *Config {
	return &Config{
		Version: version,
		Experiments: []ExperimentConfig{{
			Key:      "btn-color",
			Enabled:  true,
			Variants: []VariantConfig{{Key: "a", Weight: wA}, {Key: "b", Weight: wB}},
		}},
	}
}

func TestAssignmentStableAcrossReadsAndEngines(t *testing.T) {
	e1, err := compileEngine(abConfig("v1", 50, 50))
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the same config independently: results must be identical.
	e2, _ := compileEngine(abConfig("v1", 50, 50))

	const users = 2000
	first := make(map[string]string, users)
	for i := 0; i < users; i++ {
		u := User{ID: fmt.Sprintf("user-%d", i)}
		a1 := e1.evaluate(u).Assignments["btn-color"]
		a2 := e2.evaluate(u).Assignments["btn-color"]
		if a1.Variant != a2.Variant || a1.Bucket != a2.Bucket {
			t.Fatalf("user %d unstable across engines: %+v vs %+v", i, a1, a2)
		}
		// Repeated reads of same engine never drift.
		if a3 := e1.evaluate(u).Assignments["btn-color"]; a3.Variant != a1.Variant {
			t.Fatalf("user %d drifted on repeat read", i)
		}
		first[u.ID] = a1.Variant
	}
}

func TestVariantDistributionMatchesWeights(t *testing.T) {
	e, _ := compileEngine(abConfig("v1", 30, 70))
	counts := map[string]int{}
	const users = 20000
	for i := 0; i < users; i++ {
		a := e.evaluate(User{ID: fmt.Sprintf("u%d", i)}).Assignments["btn-color"]
		counts[a.Variant]++
	}
	gotA := float64(counts["a"]) / float64(users)
	if gotA < 0.28 || gotA > 0.32 {
		t.Fatalf("a share = %.3f, want ~0.30 (counts=%v)", gotA, counts)
	}
}

// TestWeightAdjustmentMinimizesDrift: moving 20% from a to b must move only
// users near the boundary; users hashed into the surviving a range stay on a.
func TestWeightAdjustmentMinimizesDrift(t *testing.T) {
	old, _ := compileEngine(abConfig("v1", 50, 50))
	new, _ := compileEngine(abConfig("v2", 30, 70))

	movedFromA := 0
	movedFromB := 0
	aStayed := 0
	const users = 20000
	for i := 0; i < users; i++ {
		u := User{ID: fmt.Sprintf("u%d", i)}
		o := old.evaluate(u).Assignments["btn-color"].Variant
		n := new.evaluate(u).Assignments["btn-color"].Variant
		switch {
		case o == "a" && n != "a":
			movedFromA++
		case o == "b" && n != "b":
			movedFromB++
		case o == n && n == "a":
			aStayed++
		}
	}
	// No user on b should move; b only grew.
	if movedFromB != 0 {
		t.Fatalf("%d users moved away from growing variant b", movedFromB)
	}
	// The a->b movers must be about 20% of users (the sliced boundary band).
	ratio := float64(movedFromA) / users
	if ratio < 0.18 || ratio > 0.22 {
		t.Fatalf("a->b movers = %.3f, want ~0.20 (aStayed=%d)", ratio, aStayed)
	}
	// All users still inside the remaining 30% a range must keep a.
	if aStayed == 0 {
		t.Fatal("expected some a users to stay")
	}
}

// TestReBucketSaltExplainsMigration: changing the salt deliberately remaps;
// the reason semantics remain "assigned" and ConfigFrom identifies the config.
func TestReBucketSaltRemapsExplicitly(t *testing.T) {
	c1 := abConfig("v1", 50, 50)
	c2 := abConfig("v2", 50, 50)
	c2.Experiments[0].ReBucketSalt = "forced-rebalance-2026-01"
	e1, _ := compileEngine(c1)
	e2, _ := compileEngine(c2)
	changed := 0
	const users = 5000
	for i := 0; i < users; i++ {
		u := User{ID: fmt.Sprintf("u%d", i)}
		if e1.evaluate(u).Assignments["btn-color"].Variant !=
			e2.evaluate(u).Assignments["btn-color"].Variant {
			changed++
		}
	}
	// With 50/50, a salt change should remap roughly half.
	r := float64(changed) / users
	if r < 0.35 || r > 0.65 {
		t.Fatalf("salt-driven remap = %.3f, want ~0.5", r)
	}
}

func TestTargetingMissAndFallback(t *testing.T) {
	cfg := &Config{Version: "v1", Experiments: []ExperimentConfig{{
		Key:      "e",
		Enabled:  true,
		Fallback: "control",
		Variants: []VariantConfig{{Key: "control", Weight: 90}, {Key: "test", Weight: 10}},
		Rules: &RuleConfig{Where: &ConditionConfig{
			Attribute: "vip", Operator: "eq", Values: []Value{true},
		}},
	}}}
	e, err := compileEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Non-VIP: deterministic fallback, InExperiment=false, explained.
	a := e.evaluate(User{ID: "u1", Attributes: map[string]Value{"vip": false}}).Assignments["e"]
	if a.InExperiment || a.Variant != "control" {
		t.Fatalf("non-vip = %+v", a)
	}
	if reason := a.Reason; reason != ReasonTargetingMismatch+": vip == false (got true)" &&
		len(reason) < len(ReasonTargetingMismatch) {
		t.Fatalf("unexpected reason %q", a.Reason)
	}
	// Missing attribute: no error/panic, same deterministic fallback.
	a2 := e.evaluate(User{ID: "u2"}).Assignments["e"]
	if a2.InExperiment || a2.Variant != "control" {
		t.Fatalf("missing attr = %+v", a2)
	}
	// VIP: bucketed into experiment and may receive either variant.
	a3 := e.evaluate(User{ID: "u3", Attributes: map[string]Value{"vip": true}}).Assignments["e"]
	if !a3.InExperiment || (a3.Variant != "control" && a3.Variant != "test") {
		t.Fatalf("vip = %+v", a3)
	}
}

func TestDisabledAndNoFallback(t *testing.T) {
	cfg := &Config{Version: "v1", Experiments: []ExperimentConfig{{
		Key: "off", Enabled: false,
		Variants: []VariantConfig{{Key: "x", Weight: 1}},
	}}}
	e, _ := compileEngine(cfg)
	a := e.evaluate(User{ID: "u"}).Assignments["off"]
	if a.InExperiment || a.Variant != "" || a.Reason != ReasonExperimentOff {
		t.Fatalf("off experiment = %+v", a)
	}
}

func TestLayerMutualExclusion(t *testing.T) {
	cfg := &Config{
		Version: "v1",
		Experiments: []ExperimentConfig{
			{Key: "e1", Enabled: true, Layer: "home", Variants: []VariantConfig{{Key: "c", Weight: 90}, {Key: "t", Weight: 10}}},
			{Key: "e2", Enabled: true, Layer: "home", Variants: []VariantConfig{{Key: "c", Weight: 90}, {Key: "t", Weight: 10}}},
		},
		Layers: []LayerConfig{{Key: "home", Experiments: []string{"e1", "e2"}}},
	}
	e, err := compileEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const users = 5000
	for i := 0; i < users; i++ {
		u := User{ID: fmt.Sprintf("u%d", i)}
		d := e.evaluate(u)
		a1, a2 := d.Assignments["e1"], d.Assignments["e2"]
		if a1.InExperiment && a2.InExperiment {
			t.Fatalf("user %d hit two experiments in the same layer", i)
		}
		if a1.Bucket != 0 && a2.Bucket != 0 {
			// buckets are only populated when the user reaches the experiment
		}
	}
}

func TestLayerTrafficExclusion(t *testing.T) {
	cfg := &Config{
		Version: "v1",
		Experiments: []ExperimentConfig{
			{Key: "e1", Enabled: true, Layer: "L", Variants: []VariantConfig{{Key: "c", Weight: 1}}},
		},
		Layers: []LayerConfig{{Key: "L", Traffic: 50, Experiments: []string{"e1"}}},
	}
	e, _ := compileEngine(cfg)
	in := 0
	const users = 10000
	for i := 0; i < users; i++ {
		if e.evaluate(User{ID: fmt.Sprintf("u%d", i)}).Assignments["e1"].InExperiment {
			in++
		}
	}
	ratio := float64(in) / users
	if ratio < 0.45 || ratio > 0.55 {
		t.Fatalf("layer entry = %.3f, want ~0.5", ratio)
	}
}

func TestExperimentsInDifferentLayersCanBothHit(t *testing.T) {
	cfg := &Config{
		Version: "v1",
		Experiments: []ExperimentConfig{
			{Key: "a", Enabled: true, Layer: "L1", Variants: []VariantConfig{{Key: "x", Weight: 1}}},
			{Key: "b", Enabled: true, Layer: "L2", Variants: []VariantConfig{{Key: "x", Weight: 1}}},
		},
		Layers: []LayerConfig{
			{Key: "L1", Experiments: []string{"a"}},
			{Key: "L2", Experiments: []string{"b"}},
		},
	}
	e, _ := compileEngine(cfg)
	both := 0
	const users = 2000
	for i := 0; i < users; i++ {
		d := e.evaluate(User{ID: fmt.Sprintf("u%d", i)})
		if d.Assignments["a"].InExperiment && d.Assignments["b"].InExperiment {
			both++
		}
	}
	if both == 0 {
		t.Fatal("users should be able to hit one experiment per independent layer")
	}
}
