package split

import (
	"context"
	"testing"
)

func testConfig() *Config {
	return &Config{
		Version: "v1",
		Experiments: []Experiment{
			{
				ID: "exp1",
				Variants: []Variant{
					{Name: "control", Weight: 1},
					{Name: "treat", Weight: 1},
				},
				TrafficPercent: 100,
			},
			{
				ID: "exp2",
				Variants: []Variant{
					{Name: "on", Weight: 1},
				},
				TrafficPercent: 50,
				Rules: []TargetingRule{{
					Name: "ios",
					Conditions: [][]Condition{
						{{Attribute: "os", Operator: OpEq, Values: []string{"ios"}}},
					},
				}},
			},
		},
	}
}

func TestAllocationStable(t *testing.T) {
	s, err := New(testConfig(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first := map[string]string{}
	for i := 0; i < 5000; i++ {
		u := userN(i)
		al, err := s.Allocate(ctx, "exp1", u, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !al.InExperiment {
			t.Fatalf("%s not enrolled at 100%%: %s", u, al.Reason)
		}
		first[u] = al.Variant
	}
	// Repeated calls, same and new Splitter instance, must agree.
	s2, _ := New(testConfig(), Options{})
	for i := 0; i < 5000; i++ {
		u := userN(i)
		al, _ := s.Allocate(ctx, "exp1", u, nil)
		al2, _ := s2.Allocate(ctx, "exp1", u, nil)
		if al.Variant != first[u] || al2.Variant != first[u] {
			t.Fatalf("drift for %s: %s %s first=%s", u, al.Variant, al2.Variant, first[u])
		}
	}
}

func TestTrafficPercentGate(t *testing.T) {
	s, _ := New(testConfig(), Options{})
	enrolled := 0
	const n = 10000
	for i := 0; i < n; i++ {
		al, _ := s.Allocate(context.Background(), "exp2", userN(i), Attributes{"os": "ios"})
		if al.InExperiment {
			enrolled++
		}
	}
	frac := float64(enrolled) / n
	if frac < 0.45 || frac > 0.55 {
		t.Fatalf("traffic gate enrolled %.2f, want ~0.5", frac)
	}
}

func TestTargetingSafeFallback(t *testing.T) {
	s, _ := New(testConfig(), Options{})
	ctx := context.Background()
	// Missing attributes must not error: safe fallback is not enrolled.
	al, err := s.Allocate(ctx, "exp2", "u-1", nil)
	if err != nil {
		t.Fatalf("nil attrs returned error: %v", err)
	}
	if al.InExperiment || al.Reason == "" {
		t.Fatalf("expected explainable fallback, got %+v", al)
	}
	// Wrong platform.
	if al, _ := s.Allocate(ctx, "exp2", "u-1", Attributes{"os": "android"}); al.InExperiment {
		t.Fatal("android user should not match ios rule")
	}
	// Matching platform, 50% gate is deterministic for this user.
	al3, _ := s.Allocate(ctx, "exp2", "u-1", Attributes{"os": "ios"})
	al4, _ := s.Allocate(ctx, "exp2", "u-1", Attributes{"os": "ios"})
	if al3.InExperiment != al4.InExperiment {
		t.Fatal("targeting decision drifted for same user")
	}
	// Absent experiment.
	if _, err := s.Allocate(ctx, "nope", "u-1", nil); err != ErrExperimentGone {
		t.Fatalf("want ErrExperimentGone, got %v", err)
	}
}

func TestDisabledExperiment(t *testing.T) {
	cfg := testConfig()
	cfg.Version = "v-off"
	cfg.Experiments[0].Disabled = true
	s, _ := New(cfg, Options{})
	al, err := s.Allocate(context.Background(), "exp1", "u-1", nil)
	if err != nil || al.InExperiment {
		t.Fatalf("disabled experiment must yield fallback, got %+v err=%v", al, err)
	}
}

func TestMalformedNumericConditionFallback(t *testing.T) {
	cfg := &Config{
		Version: "v",
		Experiments: []Experiment{{
			ID: "e", TrafficPercent: 100,
			Variants: []Variant{{Name: "a", Weight: 1}},
			Rules: []TargetingRule{{Conditions: [][]Condition{
				{{Attribute: "age", Operator: OpGT, Values: []string{"18"}}},
			}}},
		}},
	}
	s, _ := New(cfg, Options{})
	al, err := s.Allocate(context.Background(), "e", "u", Attributes{"age": "not-a-number"})
	if err != nil {
		t.Fatalf("malformed attr must not error: %v", err)
	}
	if al.InExperiment {
		t.Fatal("malformed numeric attr must not enroll")
	}
}

func userN(i int) string {
	return "user-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
