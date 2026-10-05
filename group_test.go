package split

import (
	"context"
	"fmt"
	"testing"
)

func mutexConfig(version string, pA, pB int) *Config {
	return &Config{
		Version: version,
		Experiments: []Experiment{
			{ID: "a", Variants: []Variant{{Name: "x", Weight: 1}}, TrafficPercent: pA},
			{ID: "b", Variants: []Variant{{Name: "y", Weight: 1}}, TrafficPercent: pB},
			{ID: "solo", Variants: []Variant{{Name: "z", Weight: 1}}, TrafficPercent: 100},
		},
		Groups: []MutexGroup{{ID: "g1", ExperimentIDs: []string{"a", "b"}}},
	}
}

func TestMutexGroupExclusivity(t *testing.T) {
	s, _ := New(mutexConfig("v1", 30, 30), Options{})
	ctx := context.Background()
	const n = 20000
	hits := map[string]int{}
	for i := 0; i < n; i++ {
		u := userN(i)
		hit := ""
		for _, id := range []string{"a", "b"} {
			al, err := s.Allocate(ctx, id, u, nil)
			if err != nil {
				t.Fatal(err)
			}
			if al.InExperiment {
				if hit != "" {
					t.Fatalf("user %s hits both %s and %s in mutex group", u, hit, id)
				}
				hit = id
			}
		}
		hits[hit]++
	}
	// a covers buckets [0,30), b covers [30,60); 40% hit neither.
	if f := float64(hits["a"]) / n; f < 0.26 || f > 0.34 {
		t.Fatalf("group a hit %.2f, want ~0.30", f)
	}
	if f := float64(hits["b"]) / n; f < 0.26 || f > 0.34 {
		t.Fatalf("group b hit %.2f, want ~0.30", f)
	}
	if f := float64(hits[""]) / n; f < 0.36 || f > 0.44 {
		t.Fatalf("neither hit %.2f, want ~0.40", f)
	}
}

func TestLayerExclusivity(t *testing.T) {
	cfg := &Config{
		Version: "v",
		Experiments: []Experiment{
			{ID: "l1", Layer: "L", Variants: []Variant{{Name: "x", Weight: 1}}, TrafficPercent: 40},
			{ID: "l2", Layer: "L", Variants: []Variant{{Name: "y", Weight: 1}}, TrafficPercent: 40},
		},
	}
	s, _ := New(cfg, Options{})
	for i := 0; i < 5000; i++ {
		u := userN(i)
		var inA, inB bool
		if al, _ := s.Allocate(context.Background(), "l1", u, nil); al.InExperiment {
			inA = true
		}
		if al, _ := s.Allocate(context.Background(), "l2", u, nil); al.InExperiment {
			inB = true
		}
		if inA && inB {
			t.Fatalf("layer exclusivity violated for %s", u)
		}
	}
}

func TestGroupTrafficOverfullRejected(t *testing.T) {
	cfg := mutexConfig("vbad", 60, 60)
	if _, err := New(cfg, Options{}); err == nil {
		t.Fatal("group traffic >100 must be rejected")
	}
}

// Changing only variant weights must preserve assignments for every
// user whose winning variant still can win; only affected users move,
// and the move direction is explainable (toward the grown variant).
func TestWeightAdjustmentMinimalDrift(t *testing.T) {
	v1 := &Config{
		Version: "v1",
		Experiments: []Experiment{{
			ID: "exp", TrafficPercent: 100,
			Variants: []Variant{{Name: "control", Weight: 1}, {Name: "treat", Weight: 1}},
		}},
	}
	s, _ := New(v1, Options{})
	before := map[string]string{}
	const n = 20000
	for i := 0; i < n; i++ {
		u := userN(i)
		al, _ := s.Allocate(context.Background(), "exp", u, nil)
		before[u] = al.Variant
	}
	v2 := &Config{
		Version: "v2",
		Experiments: []Experiment{{
			ID: "exp", TrafficPercent: 100,
			// control grows 50%->80%; treat shrinks 50%->20%.
			Variants: []Variant{{Name: "control", Weight: 4}, {Name: "treat", Weight: 1}},
		}},
	}
	if err := s.HotReplace(v2, "grow control"); err != nil {
		t.Fatal(err)
	}
	movedToTreat := 0
	for i := 0; i < n; i++ {
		u := userN(i)
		al, _ := s.Allocate(context.Background(), "exp", u, nil)
		if al.Variant != before[u] {
			if before[u] != "treat" || al.Variant != "control" {
				t.Fatalf("unexplained move %s->%s for %s", before[u], al.Variant, u)
			}
			movedToTreat++
		}
	}
	// Expect ~30% movement (the minimum necessary), never a reshuffle.
	if f := float64(movedToTreat) / n; f > 0.35 {
		t.Fatalf("weight change moved %.2f users, expected ~0.30 max", f)
	}
}

// Bumping Salt is the explicit re-bucket migration: users are expected
// to reshuffle, and the reason is visible in the config history.
func TestSaltChangeForcesRebucket(t *testing.T) {
	mk := func(version, salt string) *Config {
		return &Config{
			Version: version,
			Experiments: []Experiment{{
				ID: "exp", Salt: salt, TrafficPercent: 100,
				Variants: []Variant{{Name: "control", Weight: 1}, {Name: "treat", Weight: 1}},
			}},
		}
	}
	s, _ := New(mk("v1", ""), Options{})
	before := map[string]string{}
	for i := 0; i < 5000; i++ {
		u := userN(i)
		al, _ := s.Allocate(context.Background(), "exp", u, nil)
		before[u] = al.Variant
	}
	if err := s.HotReplace(mk("v2", "rebucket-2026-10-05"), "explicit salt bump"); err != nil {
		t.Fatal(err)
	}
	changed := 0
	for i := 0; i < 5000; i++ {
		u := userN(i)
		al, _ := s.Allocate(context.Background(), "exp", u, nil)
		if al.Variant != before[u] {
			changed++
		}
	}
	if frac := float64(changed) / 5000; frac < 0.35 || frac > 0.65 {
		t.Fatalf("salt bump should reshuffle ~50%%, got %.2f", frac)
	}
	found := false
	for _, ev := range s.RolloutAudit() {
		if ev.Reason == "explicit salt bump" {
			found = true
		}
	}
	if !found {
		t.Fatal("salt bump reason not recorded in audit trail")
	}
}

func TestVariantRatiosObserved(t *testing.T) {
	cfg := &Config{
		Version: "v1",
		Experiments: []Experiment{{
			ID: "exp", TrafficPercent: 100,
			Variants: []Variant{{Name: "a", Weight: 1}, {Name: "b", Weight: 3}},
		}},
	}
	s, _ := New(cfg, Options{})
	st := NewRatioStats()
	for i := 0; i < 40000; i++ {
		al, _ := s.Allocate(context.Background(), "exp", fmt.Sprintf("u-%d", i), nil)
		st.Observe("v1", "exp", al.Variant, al.InExperiment)
	}
	snap := st.Snapshot("v1")
	if len(snap) != 1 {
		t.Fatalf("snapshot len %d", len(snap))
	}
	if r := snap[0].Ratio("b"); r < 0.72 || r > 0.78 {
		t.Fatalf("b ratio %.3f want ~0.75", r)
	}
	st.ForgetVersion("v1")
	if len(st.Snapshot("v1")) != 0 {
		t.Fatal("ForgetVersion should release counters")
	}
}

func TestAllocateAllMutexExclusivity(t *testing.T) {
	s, _ := New(mutexConfig("v1", 30, 30), Options{})
	for i := 0; i < 3000; i++ {
		_, all, err := s.AllocateAll(context.Background(), userN(i), nil)
		if err != nil {
			t.Fatal(err)
		}
		hits := map[string]bool{}
		for _, al := range all {
			if al.InExperiment {
				if hits[al.ExperimentID] {
					t.Fatal("duplicate hit")
				}
				hits[al.ExperimentID] = true
			}
		}
		if hits["a"] && hits["b"] {
			t.Fatalf("AllocateAll returned conflicting hits for %s: %v", userN(i), hits)
		}
	}
}
