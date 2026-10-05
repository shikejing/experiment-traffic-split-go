package split

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memSink struct {
	mu     sync.Mutex
	events []AssignmentEvent
	failN  int32
	calls  int32
}

func (m *memSink) Write(_ context.Context, events []AssignmentEvent) error {
	if n := atomic.AddInt32(&m.calls, 1); n <= atomic.LoadInt32(&m.failN) {
		return errSink
	}
	m.mu.Lock()
	m.events = append(m.events, events...)
	m.mu.Unlock()
	return nil
}

var errSink = newSinkErr()

type sinkErr struct{}

func (sinkErr) Error() string { return "sink unavailable" }

func newSinkErr() error { return sinkErr{} }

func TestRecorderDelivery(t *testing.T) {
	sink := &memSink{}
	rec, err := NewEventRecorder(sink, RecorderOptions{
		QueueSize: 128, BatchSize: 8, FlushInterval: 50 * time.Millisecond, Workers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		rec.Record(AssignmentEvent{ExperimentID: "e", UserID: userN(i), Variant: "a", InExperiment: true, ConfigVersion: "v"})
	}
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.events); got != 100 {
		t.Fatalf("sink got %d events, want 100", got)
	}
	if err := rec.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Record after close must not panic and must report failure.
	if rec.Record(AssignmentEvent{}) {
		t.Fatal("record after close should fail")
	}
}

func TestRecorderDropOnBackpressure(t *testing.T) {
	block := make(chan struct{})
	sink := EventSinkFunc(func(ctx context.Context, events []AssignmentEvent) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil
	})
	rec, _ := NewEventRecorder(sink, RecorderOptions{
		QueueSize: 4, BatchSize: 2, FlushInterval: time.Hour, Workers: 1,
	})
	accepted := 0
	for i := 0; i < 1000; i++ {
		if rec.Record(AssignmentEvent{ExperimentID: "e", UserID: userN(i)}) {
			accepted++
		}
	}
	if rec.Dropped() == 0 {
		t.Fatal("expected drops under bounded queue saturation")
	}
	if accepted > 1000 {
		t.Fatal("accepted exceeds producers")
	}
	close(block)
	if err := rec.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderSinkRetryThenDrop(t *testing.T) {
	sink := &memSink{failN: 100} // always fails
	rec, _ := NewEventRecorder(sink, RecorderOptions{
		QueueSize: 8, BatchSize: 2, FlushInterval: 20 * time.Millisecond, Workers: 1,
	})
	for i := 0; i < 10; i++ {
		rec.Record(AssignmentEvent{ExperimentID: "e", UserID: userN(i)})
	}
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.Dropped() == 0 {
		t.Fatal("failed sink batches must be counted as dropped, not buffered forever")
	}
	_ = rec.Close(context.Background())
}

func TestRatioSampler(t *testing.T) {
	s := NewRatioSampler(0.5)
	kept := 0
	const n = 20000
	for i := 0; i < n; i++ {
		e := AssignmentEvent{ExperimentID: "e", UserID: userN(i)}
		if s.Keep(e) {
			kept++
		}
	}
	if f := float64(kept) / n; f < 0.47 || f > 0.53 {
		t.Fatalf("sampled %.2f, want ~0.5", f)
	}
	// Same user must be decided consistently.
	e := AssignmentEvent{ExperimentID: "e", UserID: "stable-1"}
	first := s.Keep(e)
	for i := 0; i < 10; i++ {
		if s.Keep(e) != first {
			t.Fatal("sampling decision must be stable per user")
		}
	}
	if NewRatioSampler(2).permille != 1000 || NewRatioSampler(-1).permille != 0 {
		t.Fatal("sampler rate must be clamped to [0,1]")
	}
}

func TestRecorderEndToEndWithSplitter(t *testing.T) {
	sink := &memSink{}
	rec, _ := NewEventRecorder(sink, RecorderOptions{FlushInterval: 20 * time.Millisecond})
	s, _ := New(testConfig(), Options{Recorder: rec})
	for i := 0; i < 200; i++ {
		if _, err := s.Allocate(context.Background(), "exp1", userN(i), nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 200 {
		t.Fatalf("end-to-end events %d, want 200", len(sink.events))
	}
	if sink.events[0].ConfigVersion != "v1" {
		t.Fatalf("event version %q", sink.events[0].ConfigVersion)
	}
}
