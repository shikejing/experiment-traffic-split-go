package experiment

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memSink struct {
	mu     sync.Mutex
	events []AssignmentEvent
	fail   bool
}

func (s *memSink) Write(evs []AssignmentEvent) error {
	s.mu.Lock()
	s.events = append(s.events, evs...)
	s.mu.Unlock()
	return nil
}

func (s *memSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func TestEventQueueDeliversAndDrains(t *testing.T) {
	sink := &memSink{}
	q := NewEventQueue(EventQueueOptions{Capacity: 256, Sink: sink, Sampling: 1})
	for i := 0; i < 100; i++ {
		q.Enqueue(AssignmentEvent{UserID: "u", Experiment: "e", Variant: "a"})
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if sink.len() != 100 {
		t.Fatalf("delivered %d, want 100", sink.len())
	}
}

func TestEventQueueSamplingDeterministic(t *testing.T) {
	sink := &memSink{}
	q := NewEventQueue(EventQueueOptions{Capacity: 1000, Sink: sink, Sampling: 0.5})
	// Same (experiment,user) must always make the same sampling decision.
	for round := 0; round < 50; round++ {
		q.Enqueue(AssignmentEvent{UserID: "u42", Experiment: "e", Variant: "a"})
	}
	_ = q.Close()
	n := sink.len()
	if n != 0 && n != 50 {
		t.Fatalf("per-key sampling inconsistent: delivered %d/50", n)
	}
}

func TestEventQueueSamplingRate(t *testing.T) {
	sink := &memSink{}
	q := NewEventQueue(EventQueueOptions{Capacity: 20000, Sink: sink, Sampling: 0.2})
	const n = 10000
	for i := 0; i < n; i++ {
		q.Enqueue(AssignmentEvent{UserID: string(rune('a'+i%26)) + "x" + itoa(i), Experiment: "e"})
	}
	_ = q.Close()
	got := float64(sink.len()) / n
	if got < 0.17 || got > 0.23 {
		t.Fatalf("sampled rate = %.3f, want ~0.2", got)
	}
}

func TestDropNewestNeverBlocks(t *testing.T) {
	block := make(chan struct{})
	var written atomic.Int64
	slow := EventSinkFunc(func(evs []AssignmentEvent) error {
		<-block // block the worker until released
		written.Add(int64(len(evs)))
		return nil
	})
	q := NewEventQueue(EventQueueOptions{Capacity: 4, Sink: slow, Policy: DropNewest, Sampling: 1})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			q.Enqueue(AssignmentEvent{UserID: "u", Experiment: "e"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked the caller under backpressure")
	}
	close(block)
	_ = q.Close()
	st := q.Stats()
	if st.Dropped == 0 {
		t.Fatal("expected drops under overflow")
	}
	if int(st.Enqueued) != 100000 {
		t.Fatalf("enqueued counter = %d", st.Enqueued)
	}
}

func TestDropOldestEvictsUnderOverflow(t *testing.T) {
	block := make(chan struct{})
	slow := EventSinkFunc(func(evs []AssignmentEvent) error {
		<-block
		return nil
	})
	q := NewEventQueue(EventQueueOptions{Capacity: 4, Sink: slow, Policy: DropOldest, Sampling: 1})
	accepted := 0
	for i := 0; i < 10000; i++ {
		if q.Enqueue(AssignmentEvent{UserID: "u", Experiment: "e"}) {
			accepted++
		}
	}
	if accepted == 0 {
		t.Fatal("drop-oldest should keep accepting newest events")
	}
	if q.Stats().Dropped == 0 {
		t.Fatal("expected evictions to be counted")
	}
	close(block)
	_ = q.Close()
}

func TestNilSinkSilentlyConsumes(t *testing.T) {
	q := NewEventQueue(EventQueueOptions{Capacity: 8, Sampling: 1})
	if !q.Enqueue(AssignmentEvent{UserID: "u", Experiment: "e"}) {
		t.Fatal("nil-sink queue should accept")
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
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
