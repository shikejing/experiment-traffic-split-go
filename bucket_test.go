package split

import (
	"fmt"
	"testing"
)

func TestHash100Deterministic(t *testing.T) {
	if got := hash100("user-1"); got != hash100("user-1") {
		t.Fatal("hash100 not deterministic")
	}
	if hash100("a") == hash100("b") {
		t.Fatal("hash100 collision on distinct keys")
	}
	for i := 0; i < 10000; i++ {
		if b := hash100(fmt.Sprintf("u-%d", i)); b >= 100 {
			t.Fatalf("bucket out of range: %d", b)
		}
	}
}

func TestWeightedPickDistribution(t *testing.T) {
	variants := []Variant{{Name: "a", Weight: 1}, {Name: "b", Weight: 1}, {Name: "c", Weight: 2}}
	const n = 60000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		v := weightedPick("e|s|", fmt.Sprintf("user-%d", i), variants)
		counts[v]++
	}
	want := map[string]float64{"a": 0.25, "b": 0.25, "c": 0.5}
	for name, w := range want {
		got := float64(counts[name]) / n
		if d := got - w; d > 0.02 || d < -0.02 {
			t.Fatalf("variant %s ratio %.3f, want %.3f (counts=%v)", name, got, w, counts)
		}
	}
}

// Weight adjustments must not move users whose variant keeps the top
// score. Increasing a variant only pulls users into it; shrinking only
// moves users that can no longer stay.
func TestWeightChangeStability(t *testing.T) {
	old := []Variant{{Name: "control", Weight: 1}, {Name: "treat", Weight: 1}}
	newCfg := []Variant{{Name: "control", Weight: 3}, {Name: "treat", Weight: 1}}
	const n = 20000
	oldAssign := make(map[string]string, n)
	for i := 0; i < n; i++ {
		u := fmt.Sprintf("stable-user-%d", i)
		oldAssign[u] = weightedPick("e|s|", u, old)
	}
	moved := 0
	controlStayed := 0
	for i := 0; i < n; i++ {
		u := fmt.Sprintf("stable-user-%d", i)
		newV := weightedPick("e|s|", u, newCfg)
		if newV != oldAssign[u] {
			moved++
			// Only treat->control movement is expected when control grows.
			if oldAssign[u] != "treat" || newV != "control" {
				t.Fatalf("unexpected migration %s -> %s for %s", oldAssign[u], newV, u)
			}
		} else if newV == "control" {
			controlStayed++
		}
	}
	if controlStayed == 0 {
		t.Fatal("no control users stayed put")
	}
	// At most ~25% of users need to move when going from 50/50 to 75/25.
	if frac := float64(moved) / n; frac > 0.30 {
		t.Fatalf("too much drift after weight change: %.2f", frac)
	}
}

func TestWeightedPickAllZero(t *testing.T) {
	if v := weightedPick("e|s|", "u", []Variant{{Name: "a", Weight: 0}}); v != "" {
		t.Fatalf("expected empty pick, got %q", v)
	}
}
