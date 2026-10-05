package experiment

import (
	"sync"
	"sync/atomic"
	"time"
)

// AssignmentEvent is recorded (and optionally sampled) for each assignment.
type AssignmentEvent struct {
	Timestamp    time.Time
	UserID       string
	Experiment   string
	Variant      string
	InExperiment bool
	Reason       string
	Bucket       uint32
	ConfigFrom   string
}

// EventSink consumes event batches; implementations must be safe for
// concurrent invocation semantics (the queue calls Write from one worker).
// Write should return promptly: a slow sink causes the bounded queue to fill
// and events are then dropped per the configured policy.
type EventSink interface {
	Write(events []AssignmentEvent) error
}

// EventSinkFunc adapts a function to an EventSink.
type EventSinkFunc func(events []AssignmentEvent) error

// Write implements EventSink.
func (f EventSinkFunc) Write(events []AssignmentEvent) error { return f(events) }

// DropPolicy defines behavior when the event queue is full.
type DropPolicy int

const (
	// DropNewest drops the incoming event (never blocks the assign path).
	DropNewest DropPolicy = iota
	// DropOldest evicts one queued event to make room (never blocks).
	DropOldest
)

const (
	defaultQueueCapacity = 4096
	flushBatchSize       = 256
	flushInterval        = 100 * time.Millisecond
)

// EventQueue is a bounded, lossy, single-worker fan-out buffer. Enqueue never
// blocks the caller; overflow is handled per DropPolicy and counted so that
// backpressure is observable.
type EventQueue struct {
	ch     chan AssignmentEvent
	sink   EventSink
	policy DropPolicy
	rate   float64
	stopCh chan struct{}
	wg     sync.WaitGroup

	enqueued   atomic.Uint64
	dropped    atomic.Uint64
	written    atomic.Uint64
	sampledOut atomic.Uint64
}

// EventQueueOptions configures an EventQueue.
type EventQueueOptions struct {
	Capacity int       // <=0 uses defaultQueueCapacity
	Sink     EventSink // nil queue discards accepted events silently
	Policy   DropPolicy
	Sampling float64 // in [0,1]; 1 records everything, <=0 nothing
}

// NewEventQueue creates and starts an event queue.
func NewEventQueue(opts EventQueueOptions) *EventQueue {
	capacity := opts.Capacity
	if capacity <= 0 {
		capacity = defaultQueueCapacity
	}
	q := &EventQueue{
		ch:     make(chan AssignmentEvent, capacity),
		sink:   opts.Sink,
		policy: opts.Policy,
		rate:   opts.Sampling,
		stopCh: make(chan struct{}),
	}
	q.wg.Add(1)
	go q.worker()
	return q
}

// sampled applies deterministic per (experiment, user) sampling, so a given
// user in a given experiment is either consistently recorded or skipped.
func (q *EventQueue) sampled(ev AssignmentEvent) bool {
	switch {
	case q.rate >= 1:
		return true
	case q.rate <= 0:
		return false
	default:
		b := hashUser("", "assignment-sampling", ev.Experiment+"|"+ev.UserID)
		return float64(b) < q.rate*float64(Scale)
	}
}

// Enqueue is non-blocking under every backpressure condition. It reports
// whether the event was accepted into the queue; unsampled events count as
// accepted (they are intentionally not recorded).
func (q *EventQueue) Enqueue(ev AssignmentEvent) bool {
	if !q.sampled(ev) {
		q.sampledOut.Add(1)
		return true
	}
	q.enqueued.Add(1)

	if q.policy == DropOldest {
		for {
			select {
			case q.ch <- ev:
				return true
			default:
			}
			// Full: evict one pending event, then retry once.
			select {
			case <-q.ch:
				q.dropped.Add(1)
				continue // retry the send
			default:
				// Another goroutine drained the slot; loop retries the send.
				continue
			}
		}
	}

	select {
	case q.ch <- ev:
		return true
	default:
		q.dropped.Add(1)
		return false
	}
}

// worker batches queued events to the sink and guarantees a final drain.
func (q *EventQueue) worker() {
	defer q.wg.Done()
	batch := make([]AssignmentEvent, 0, flushBatchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if q.sink != nil {
			if err := q.sink.Write(batch); err != nil {
				q.dropped.Add(uint64(len(batch)))
			} else {
				q.written.Add(uint64(len(batch)))
			}
		} else {
			q.written.Add(uint64(len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case ev := <-q.ch:
			batch = append(batch, ev)
			if len(batch) >= flushBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-q.stopCh:
			// Drain everything that was admitted before stopping.
			for {
				select {
				case ev := <-q.ch:
					batch = append(batch, ev)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Close stops the worker after admitting a full drain; it is safe to call
// concurrently with Enqueue (post-close enqueues are dropped).
func (q *EventQueue) Close() error {
	select {
	case <-q.stopCh:
		q.wg.Wait()
		return nil
	default:
		close(q.stopCh)
		q.wg.Wait()
		return nil
	}
}

// QueueStats reports counters for backpressure observability.
type QueueStats struct {
	Enqueued   uint64 // sampled-in events offered to the queue
	Dropped    uint64 // evicted/rejected due to overflow or sink failure
	Written    uint64 // delivered to the sink
	SampledOut uint64 // intentionally skipped by sampling
	InFlight   int    // currently buffered
}

// Stats returns a snapshot of queue counters.
func (q *EventQueue) Stats() QueueStats {
	return QueueStats{
		Enqueued:   q.enqueued.Load(),
		Dropped:    q.dropped.Load(),
		Written:    q.written.Load(),
		SampledOut: q.sampledOut.Load(),
		InFlight:   len(q.ch),
	}
}
