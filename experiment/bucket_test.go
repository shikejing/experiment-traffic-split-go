package experiment

import "testing"

func TestBuildRangesCoverageAndOrder(t *testing.T) {
	rs, total, err := buildRanges([]VariantConfig{
		{Key: "a", Weight: 50},
		{Key: "b", Weight: 50},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 100 {
		t.Fatalf("total = %d, want 100", total)
	}
	if rs[0].From != 0 || rs[0].To != Scale/2 {
		t.Fatalf("a range = [%d,%d), want [0,%d)", rs[0].From, rs[0].To, Scale/2)
	}
	if rs[1].From != Scale/2 || rs[1].To != Scale {
		t.Fatalf("b range = [%d,%d), want [%d,%d)", rs[1].From, rs[1].To, Scale/2, Scale)
	}
	// Every bucket maps to exactly one variant.
	counts := map[string]int{}
	for b := uint32(0); b < Scale; b++ {
		k := pickVariant(rs, b)
		if k == "" {
			t.Fatalf("bucket %d uncovered", b)
		}
		counts[k]++
	}
	if counts["a"] != int(Scale/2) || counts["b"] != int(Scale/2) {
		t.Fatalf("counts = %v", counts)
	}
}

func TestBuildRangesRelativeWeights(t *testing.T) {
	rs, _, err := buildRanges([]VariantConfig{
		{Key: "a", Weight: 1},
		{Key: "b", Weight: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].To != uint32(uint64(Scale)/4) {
		t.Fatalf("a boundary = %d, want %d", rs[0].To, Scale/4)
	}
	if rs[1].To != Scale {
		t.Fatalf("b end must pin to Scale, got %d", rs[1].To)
	}
}

func TestBuildRangesErrors(t *testing.T) {
	cases := []struct {
		name string
		vars []VariantConfig
		want error
	}{
		{"empty", nil, errNoVariants},
		{"empty key", []VariantConfig{{Key: "", Weight: 1}}, errEmptyVariantKey},
		{"negative", []VariantConfig{{Key: "a", Weight: -1}}, errNegativeWeight},
		{"zero total", []VariantConfig{{Key: "a", Weight: 0}}, errZeroTotalWeight},
		{"duplicate", []VariantConfig{{Key: "a", Weight: 1}, {Key: "a", Weight: 1}}, errDuplicateVariant},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := buildRanges(c.vars); err != c.want {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// TestRangesWeightAdjustmentStability captures the anti-drift contract of the
// cumulative-range layout: when control shrinks from 50% to 20%, its NEW
// interval [0,2000) is a prefix of the old one, so every user whose hash sits
// in that prefix keeps control; only users in the moved boundary band
// [2000,5000) migrate. Appending variants never shifts existing starts.
func TestRangesWeightAdjustmentStability(t *testing.T) {
	old, _, _ := buildRanges([]VariantConfig{
		{Key: "control", Weight: 50},
		{Key: "treat", Weight: 50},
	})
	shrunk, _, _ := buildRanges([]VariantConfig{
		{Key: "control", Weight: 20},
		{Key: "treat", Weight: 80},
	})
	if shrunk[0].From != old[0].From {
		t.Fatalf("control start drifted: %d -> %d", old[0].From, shrunk[0].From)
	}
	if !(shrunk[0].To <= old[0].To) {
		t.Fatal("shrinking control must move its right boundary left")
	}
	// Appending a new variant must not move existing starts.
	three, _, _ := buildRanges([]VariantConfig{
		{Key: "control", Weight: 50},
		{Key: "treat", Weight: 40},
		{Key: "treat2", Weight: 10},
	})
	if three[0].From != old[0].From || three[0].To != old[0].To {
		t.Fatalf("control boundary drifted after append: %+v vs %+v", three[0], old[0])
	}
	if three[1].From != old[1].From {
		t.Fatalf("treat start drifted: %d vs %d", three[1].From, old[1].From)
	}
}

// TestStableUsersSurviveShrink verifies at the hash level that nobody inside
// the surviving control prefix migrates.
func TestStableUsersSurviveShrink(t *testing.T) {
	old, _, _ := buildRanges([]VariantConfig{
		{Key: "control", Weight: 50}, {Key: "treat", Weight: 50},
	})
	next, _, _ := buildRanges([]VariantConfig{
		{Key: "control", Weight: 20}, {Key: "treat", Weight: 80},
	})
	for b := uint32(0); b < next[0].To; b++ {
		if pickVariant(old, b) != "control" || pickVariant(next, b) != "control" {
			t.Fatalf("bucket %d migrated despite staying inside control prefix", b)
		}
	}
}

func TestHashStableAndDeterministic(t *testing.T) {
	h1 := hashUser("salt", "ns", "user-42")
	h2 := hashUser("salt", "ns", "user-42")
	if h1 != h2 {
		t.Fatal("hash not deterministic")
	}
	if h1 >= Scale {
		t.Fatal("hash out of range")
	}
	if hashUser("", "", "") == hashUser("", "", "x") {
		// Not strictly impossible, but empty vs x colliding would be suspicious.
		t.Log("empty and x hashed equal (unlikely)")
	}
	// Different namespace must (overwhelmingly) differ.
	if hashUser("", "ns-a", "u") == hashUser("", "ns-b", "u") {
		t.Fatal("namespaces collided")
	}
}
