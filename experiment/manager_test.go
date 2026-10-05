package experiment

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestManager(t *testing.T, cfg *Config) *Manager {
	t.Helper()
	m, err := NewManager(cfg, ManagerOptions{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestNewManagerRejectsInvalidConfig(t *testing.T) {
	bad := &Config{Version: "v1", Experiments: []ExperimentConfig{{
		Key: "e", Enabled: true, Variants: []VariantConfig{{Key: "x", Weight: 0}},
	}}}
	if _, err := NewManager(bad, ManagerOptions{}); err == nil {
		t.Fatal("zero-weight config must be rejected")
	}
}

func TestHotUpdateAndAtomicSwitch(t *testing.T) {
	m := newTestManager(t, abConfig("v1", 50, 50))
	if m.Snapshot().StableVersion != "v1" {
		t.Fatal("initial version mismatch")
	}
	if err := m.Update(abConfig("v2", 50, 50)); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().StableVersion != "v2" {
		t.Fatal("version did not switch")
	}
	// Same version rejected.
	if err := m.Update(abConfig("v2", 50, 50)); err != ErrNoChange {
		t.Fatalf("want ErrNoChange, got %v", err)
	}
}

func TestUpdateFailureKeepsPreviousConfig(t *testing.T) {
	m := newTestManager(t, abConfig("v1", 50, 50))
	before := m.Variant(User{ID: "u123"}, "btn-color")

	bad := &Config{Version: "v-bad", Experiments: []ExperimentConfig{{
		Key: "e", Enabled: true, Variants: []VariantConfig{{Key: "x", Weight: -1}},
	}}}
	if err := m.Update(bad); err == nil {
		t.Fatal("invalid update must fail")
	}
	snap := m.Snapshot()
	if snap.StableVersion != "v1" {
		t.Fatalf("active version changed after failed update: %s", snap.StableVersion)
	}
	after := m.Variant(User{ID: "u123"}, "btn-color")
	if before != after {
		t.Fatal("assignment changed after failed update")
	}
}

func TestRollbackAfterHotUpdate(t *testing.T) {
	m := newTestManager(t, abConfig("v1", 50, 50))
	if err := m.Update(abConfig("v2", 30, 70)); err != nil {
		t.Fatal(err)
	}
	if err := m.Rollback("v1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if m.Snapshot().StableVersion != "v1" {
		t.Fatal("rollback did not restore v1")
	}
	// Unknown version rollback errors.
	if err := m.Rollback("nope"); err == nil {
		t.Fatal("unknown rollback target must error")
	}
	// History records every transition.
	actions := map[string]bool{}
	for _, rec := range m.Snapshot().History {
		actions[rec.Action] = true
	}
	for _, want := range []string{"initialize", "activate", "rollback-restore"} {
		if !actions[want] {
			t.Fatalf("history missing action %q: %+v", want, m.Snapshot().History)
		}
	}
}

func TestGrayRolloutRoutingIsStable(t *testing.T) {
	v1 := abConfig("v1", 50, 50)
	v2 := abConfig("v2", 50, 50)
	// Different salt on v2 so the two versions produce distinguishable variants.
	v2.Experiments[0].ReBucketSalt = "v2salt"
	m := newTestManager(t, v1)
	if err := m.StageRollout(v2); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot().CandidateVersion; got != "v2" {
		t.Fatalf("candidate = %q", got)
	}

	// At 0%, everyone stays on v1.
	if err := m.SetRollout("v2", 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		a := m.Assignment(User{ID: fmt.Sprintf("u%d", i)}, "btn-color")
		if a.ConfigFrom != "v1" {
			t.Fatalf("at 0%% user %d came from %s", i, a.ConfigFrom)
		}
	}

	// 50%: deterministic fixed set; repeating reads never flip versions.
	if err := m.SetRollout("v2", 50); err != nil {
		t.Fatal(err)
	}
	from := map[string]string{}
	for i := 0; i < 2000; i++ {
		u := User{ID: fmt.Sprintf("u%d", i)}
		a := m.Assignment(u, "btn-color")
		from[u.ID] = a.ConfigFrom
		a2 := m.Assignment(u, "btn-color")
		if a.ConfigFrom != a2.ConfigFrom {
			t.Fatalf("user %s flipped versions on repeat read", u.ID)
		}
	}
	n2 := 0
	for _, v := range from {
		if v == "v2" {
			n2++
		}
	}
	ratio := float64(n2) / 2000
	if ratio < 0.45 || ratio > 0.55 {
		t.Fatalf("candidate share = %.3f, want ~0.5", ratio)
	}

	// Raising to 100 only adds users; prior v2 users stay on v2.
	if err := m.SetRollout("v2", 100); err != nil {
		t.Fatal(err)
	}
	for id, prev := range from {
		u := User{ID: id}
		now := m.Assignment(u, "btn-color").ConfigFrom
		if prev == "v2" && now != "v2" {
			t.Fatalf("user %s regressed from v2", id)
		}
	}

	// Promote: candidate becomes stable, rollout cleared.
	if err := m.Promote("v2"); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if snap.StableVersion != "v2" || snap.CandidateVersion != "" {
		t.Fatalf("post-promote snapshot = %+v", snap)
	}
}

func TestRollbackCandidateDuringRollout(t *testing.T) {
	v1 := abConfig("v1", 50, 50)
	v2 := abConfig("v2", 50, 50)
	m := newTestManager(t, v1)
	if err := m.StageRollout(v2); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRollout("v2", 80); err != nil {
		t.Fatal(err)
	}
	if err := m.Rollback("v2"); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if snap.CandidateVersion != "" || snap.StableVersion != "v1" {
		t.Fatalf("post-rollback = %+v", snap)
	}
	for i := 0; i < 100; i++ {
		if m.Assignment(User{ID: fmt.Sprintf("u%d", i)}, "btn-color").ConfigFrom != "v1" {
			t.Fatal("rollback did not return everyone to v1")
		}
	}
}

func TestRolloutGuards(t *testing.T) {
	m := newTestManager(t, abConfig("v1", 50, 50))
	if err := m.SetRollout("v2", 50); err != ErrNoRollout {
		t.Fatalf("want ErrNoRollout, got %v", err)
	}
	if err := m.Promote("v2"); err != ErrNoRollout {
		t.Fatalf("want ErrNoRollout, got %v", err)
	}
	if err := m.StageRollout(abConfig("v2", 50, 50)); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRollout("v2", 101); err != ErrInvalidPercent {
		t.Fatalf("want ErrInvalidPercent, got %v", err)
	}
	if err := m.StageRollout(abConfig("v3", 50, 50)); err == nil {
		t.Fatal("staging a second candidate must be rejected")
	}
	if err := m.Update(abConfig("v3", 50, 50)); err != ErrRolloutInProgress {
		t.Fatalf("want ErrRolloutInProgress, got %v", err)
	}
}

// TestConcurrentReadsNeverSeeHalfState hammers Evaluate during updates: every
// read must report exactly one coherent version and a valid variant.
func TestConcurrentReadsNeverSeeHalfState(t *testing.T) {
	m := newTestManager(t, abConfig("v1", 50, 50))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				u := User{ID: fmt.Sprintf("g%d-u%d", g, i%1000)}
				a := m.Assignment(u, "btn-color")
				if a == nil {
					t.Errorf("nil assignment during switch")
					return
				}
				if a.ConfigFrom != "v1" && a.ConfigFrom != "v2" {
					t.Errorf("impossible config version %q", a.ConfigFrom)
					return
				}
				if a.Variant != "a" && a.Variant != "b" {
					t.Errorf("impossible variant %q", a.Variant)
					return
				}
			}
		}(g)
	}

	for round := 0; round < 20; round++ {
		v := "v2"
		if round%2 == 1 {
			v = "v1"
		}
		_ = m.Update(abConfig(v, 50, 50)) // may be ErrNoChange alternating
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
}

// TestAssignmentStableAcrossHotReload: same-version rebuilds and unrelated
// changes must keep each user's variant.
func TestAssignmentStableAcrossHotReload(t *testing.T) {
	m := newTestManager(t, abConfig("v1", 50, 50))
	before := map[string]string{}
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("u%d", i)
		before[id] = m.Variant(User{ID: id}, "btn-color")
	}
	// Add an unrelated experiment and reload as a new version.
	cfg2 := abConfig("v2", 50, 50)
	cfg2.Experiments = append(cfg2.Experiments, ExperimentConfig{
		Key: "other", Enabled: true,
		Variants: []VariantConfig{{Key: "on", Weight: 1}},
	})
	if err := m.Update(cfg2); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("u%d", i)
		if got := m.Variant(User{ID: id}, "btn-color"); got != before[id] {
			t.Fatalf("user %s drifted %s -> %s after unrelated update", id, before[id], got)
		}
	}
}

func TestHistoryIsBounded(t *testing.T) {
	m := newTestManager(t, abConfig("v1", 50, 50))
	for i := 2; i <= MaxHistoryRecords+20; i++ {
		prev := "v1"
		if i > 2 {
			prev = fmt.Sprintf("v%d", i-1)
		}
		cur := fmt.Sprintf("v%d", i)
		_ = m.Update(abConfig(cur, 50, 50))
		// alternate back/forth so Update never returns ErrNoChange
		_ = m.Update(abConfig(prev, 50, 50))
	}
	if n := len(m.Snapshot().History); n > MaxHistoryRecords {
		t.Fatalf("history grew unbounded: %d", n)
	}
}
