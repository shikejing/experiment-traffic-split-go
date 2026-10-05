package experiment

import (
	"fmt"
	"sync"
	"testing"
)

var benchCfg = &Config{
	Version: "v1",
	Experiments: []ExperimentConfig{
		{Key: "e1", Enabled: true, Variants: []VariantConfig{{Key: "a", Weight: 50}, {Key: "b", Weight: 50}}},
		{Key: "e2", Enabled: true, Variants: []VariantConfig{{Key: "c", Weight: 90}, {Key: "d", Weight: 10}},
			Rules: &RuleConfig{Op: "and", All: []RuleConfig{
				{Where: &ConditionConfig{Attribute: "city", Operator: "in",
					Values: []Value{"beijing", "shanghai", "shenzhen"}}},
				{Where: &ConditionConfig{Attribute: "age", Operator: "gte", Values: []Value{18}}},
			}}},
	},
}

func BenchmarkEvaluate(b *testing.B) {
	m, err := NewManager(benchCfg, ManagerOptions{})
	if err != nil {
		b.Fatal(err)
	}
	attrs := map[string]Value{"city": "beijing", "age": 25}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			u := User{ID: fmt.Sprintf("u%d", i%10000), Attributes: attrs}
			_ = m.Evaluate(u)
			i++
		}
	})
}

// BenchmarkEvaluateDuringHotReload ensures updates do not stall readers:
// a writer goroutine repeatedly alternates config versions while readers run.
func BenchmarkEvaluateDuringHotReload(b *testing.B) {
	m, _ := NewManager(benchCfg, ManagerOptions{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		v2 := *benchCfg
		v2.Version = "v2"
		v1v := *benchCfg
		v1v.Version = "v1"
		toggle := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			toggle = !toggle
			if toggle {
				_ = m.Update(&v2)
			} else {
				_ = m.Update(&v1v)
			}
		}
	}()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = m.Variant(User{ID: fmt.Sprintf("u%d", i%10000)}, "e1")
			i++
		}
	})
	b.StopTimer()
	close(stop)
	wg.Wait()
}
