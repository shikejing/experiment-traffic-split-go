package split

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkAllocate(b *testing.B) {
	s, _ := New(testConfig(), Options{})
	attrs := Attributes{"os": "ios"}
	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _ = s.Allocate(ctx, "exp1", userN(i), attrs)
			i++
		}
	})
}

// Allocations must not drift over time or across GC cycles.
func TestStableAcrossTimeAndGC(t *testing.T) {
	s, _ := New(testConfig(), Options{})
	ctx := context.Background()
	want := map[string]string{}
	for i := 0; i < 1000; i++ {
		u := userN(i)
		al, _ := s.Allocate(ctx, "exp1", u, nil)
		want[u] = al.Variant
	}
	for cycle := 0; cycle < 5; cycle++ {
		runtime.GC()
		for i := 0; i < 1000; i++ {
			u := userN(i)
			al, _ := s.Allocate(ctx, "exp1", u, nil)
			if al.Variant != want[u] {
				t.Fatalf("drift after GC cycle %d for %s", cycle, u)
			}
		}
	}
}

// Many hot swaps must not grow retained memory: old versions can be
// forgotten, and allocations keep working during the churn.
func TestBoundedMemoryAcrossHotSwaps(t *testing.T) {
	s, _ := New(testConfig(), Options{})
	ctx := context.Background()
	var m0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	for v := 1; v <= 200; v++ {
		cfg := testConfig()
		cfg.Version = "v-" + itoa(v)
		if err := s.HotReplace(cfg, "churn"); err != nil {
			t.Fatal(err)
		}
		if v > 2 {
			_ = s.ForgetVersion("v-" + itoa(v-1))
		}
		if _, err := s.Allocate(ctx, "exp1", "u-1", nil); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	// Heap growth allowance for retained maps/audit (audit retains
	// small entries; that is expected and bounded by ramp actions).
	if growth := int64(m1.HeapAlloc) - int64(m0.HeapAlloc); growth > 4<<20 {
		t.Fatalf("heap grew by %d bytes across 200 swaps", growth)
	}
}

// Throughput during hot swaps: readers run concurrently with the swap
// stream; the churn must not cause errors and all readers must make
// progress.
func TestThroughputDuringHotSwap(t *testing.T) {
	rec, _ := NewEventRecorder(nilSink{}, RecorderOptions{})
	s, _ := New(testConfig(), Options{Recorder: rec})
	defer rec.Close(context.Background())
	ctx := context.Background()
	var wg sync.WaitGroup
	var done atomic.Bool
	var total atomic.Int64
	start := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			i := w
			for !done.Load() {
				if _, err := s.Allocate(ctx, "exp1", userN(i), nil); err != nil {
					t.Error(err)
					return
				}
				total.Add(1)
				i += 8
			}
		}(w)
	}
	close(start)
	// Measure steady allocation throughput, then perform swaps and
	// require allocation throughput to stay on the same order.
	measure := func(dur time.Duration) int64 {
		before := total.Load()
		time.Sleep(dur)
		return total.Load() - before
	}
	base := measure(50 * time.Millisecond)
	if base == 0 {
		t.Fatal("baseline made no progress")
	}
	for v := 0; v < 50; v++ {
		cfg := testConfig()
		cfg.Version = "swap-" + itoa(v)
		_ = s.HotReplace(cfg, "bench")
	}
	during := measure(50 * time.Millisecond)
	done.Store(true)
	wg.Wait()
	// Atomic swaps must not cut reader throughput by more than half on
	// this very short window (generous bound for CI noise).
	if float64(during) < 0.5*float64(base) {
		t.Fatalf("throughput collapsed during swaps: base=%d during=%d", base, during)
	}
}

type nilSink struct{}

func (nilSink) Write(_ context.Context, _ []AssignmentEvent) error { return nil }
