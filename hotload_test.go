package split

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestHotReplaceRollbackOnInvalid(t *testing.T) {
	s, _ := New(testConfig(), Options{})
	bad := &Config{Version: "bad", Experiments: []Experiment{
		{ID: "x", TrafficPercent: 100}, // no variants -> invalid
	}}
	if err := s.HotReplace(bad, "should fail"); err == nil {
		t.Fatal("invalid config must be rejected")
	}
	if v := s.CurrentVersion(); v != "v1" {
		t.Fatalf("active version = %q after failed swap, want v1", v)
	}
	al, err := s.Allocate(context.Background(), "exp1", "u-1", nil)
	if err != nil || !al.InExperiment {
		t.Fatalf("old config must keep serving after failed swap: %v", err)
	}
}

func TestStoreTryReplaceHookRollback(t *testing.T) {
	st, err := NewStore(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	before := st.Current().version
	cfg := testConfig()
	cfg.Version = "hook-v"
	hookErr := errors.New("post-swap check failed")
	if err := st.TryReplace(cfg, func(cc *compiledConfig) error { return hookErr }); !errors.Is(err, hookErr) {
		t.Fatalf("want hook error propagated, got %v", err)
	}
	if v := st.Current().version; v != before {
		t.Fatalf("want rollback to %s, got %s", before, v)
	}
}

func TestConcurrentReadsSeeWholeConfig(t *testing.T) {
	s, _ := New(testConfig(), Options{})
	ctx := context.Background()
	var wg sync.WaitGroup
	var failures atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20000; i++ {
				al, err := s.Allocate(ctx, "exp1", fmt.Sprintf("u-%d", i), nil)
				if err != nil {
					failures.Add(1)
					return
				}
				// A whole config means either enrolled with a valid
				// variant or a coherent not-enrolled reason.
				if al.InExperiment && al.Variant == "" {
					failures.Add(1)
				}
			}
		}()
	}
	for v := 1; v <= 20; v++ {
		cfg := testConfig()
		cfg.Version = fmt.Sprintf("v-%d", v)
		_ = s.HotReplace(cfg, "load test")
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d readers observed inconsistent allocation", failures.Load())
	}
}

func TestGrayRampAndRollback(t *testing.T) {
	v1 := &Config{Version: "v1", Experiments: []Experiment{{
		ID: "e", TrafficPercent: 100,
		Variants: []Variant{{Name: "old", Weight: 1}},
	}}}
	s, _ := New(v1, Options{})
	// v2 keeps same users but adds a variant with zero weight? To make
	// version observability obvious we use distinct variant names.
	v2 := &Config{Version: "v2", Experiments: []Experiment{{
		ID: "e", TrafficPercent: 100,
		Variants: []Variant{{Name: "new", Weight: 1}},
	}}}
	if err := s.PutConfig(v2); err != nil {
		t.Fatal(err)
	}
	// At 0%, everyone stays on v1.
	if err := s.Ramp("v2", 0, "start canary"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		al, _ := s.Allocate(context.Background(), "e", userN(i), nil)
		if al.Variant != "old" {
			t.Fatal("0% ramp must keep everyone on old config")
		}
	}
	// At 50%, a stable ~half uses v2; same user resolves consistently.
	if err := s.Ramp("v2", 50, "half"); err != nil {
		t.Fatal(err)
	}
	newCount := 0
	resolved := map[string]string{}
	for i := 0; i < 4000; i++ {
		u := userN(i)
		al, _ := s.Allocate(context.Background(), "e", u, nil)
		resolved[u] = al.Variant
		if al.Variant == "new" {
			newCount++
		}
	}
	if f := float64(newCount) / 4000; f < 0.45 || f > 0.55 {
		t.Fatalf("ramp 50%% served %.2f on new", f)
	}
	// Determinism under ramp.
	for i := 0; i < 4000; i++ {
		u := userN(i)
		al, _ := s.Allocate(context.Background(), "e", u, nil)
		if al.Variant != resolved[u] {
			t.Fatal("ramp resolution drifted")
		}
	}
	// Rollback: everyone returns to v1 immediately and traceably.
	if err := s.RollbackVersion("v2", "canary bad"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		al, _ := s.Allocate(context.Background(), "e", userN(i), nil)
		if al.Variant != "old" {
			t.Fatal("after rollback everyone must be on old config")
		}
	}
	actions := map[string]bool{}
	for _, ev := range s.RolloutAudit() {
		actions[ev.Action] = true
		if ev.Action == ActionRollback && ev.Reason != "canary bad" {
			t.Fatal("rollback reason not recorded")
		}
	}
	if !actions[ActionRollback] || !actions[ActionSetPercent] {
		t.Fatalf("audit missing actions: %v", actions)
	}
}

func TestPromoteSwitchesAtomically(t *testing.T) {
	v1 := &Config{Version: "v1", Experiments: []Experiment{{
		ID: "e", TrafficPercent: 100, Variants: []Variant{{Name: "old", Weight: 1}},
	}}}
	s, _ := New(v1, Options{})
	v2 := &Config{Version: "v2", Experiments: []Experiment{{
		ID: "e", TrafficPercent: 100, Variants: []Variant{{Name: "new", Weight: 1}},
	}}}
	if err := s.PutConfig(v2); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote("v2", "full release"); err != nil {
		t.Fatal(err)
	}
	if s.CurrentVersion() != "v2" {
		t.Fatalf("active = %s", s.CurrentVersion())
	}
	al, _ := s.Allocate(context.Background(), "e", "u", nil)
	if al.Variant != "new" {
		t.Fatalf("after promote variant %q", al.Variant)
	}
	if err := s.RollbackVersion("v2", "undo"); err != nil {
		t.Fatal(err)
	}
	if s.CurrentVersion() != "v1" {
		t.Fatalf("rollback active = %s", s.CurrentVersion())
	}
}
