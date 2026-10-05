package split

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// AssignmentEvent is emitted once per user/experiment assignment for
// downstream traffic-ratio statistics.
type AssignmentEvent struct {
	Time          time.Time `json:"time"`
	ExperimentID  string    `json:"experiment"`
	Variant       string    `json:"variant"`
	InExperiment  bool      `json:"in_experiment"`
	UserID        string    `json:"user_id"`
	ConfigVersion string    `json:"config_version"`
	Reason        string    `json:"reason,omitempty"`
}

// EventSink consumes batches of events (Kafka producer, file, etc.).
// Implementations must be safe for concurrent use.
type EventSink interface {
	Write(ctx context.Context, events []AssignmentEvent) error
}

// EventSinkFunc adapts a plain function to EventSink.
type EventSinkFunc func(ctx context.Context, events []AssignmentEvent) error

func (f EventSinkFunc) Write(ctx context.Context, events []AssignmentEvent) error {
	return f(ctx, events)
}

// DropPolicy defines backpressure behavior when the recorder queue is
// saturated.
type DropPolicy int

const (
	// DropNewest drops the event that cannot be enqueued. This is the
	// default: a statistics pipeline must never block or fail the
	// allocation hot path. The drop is counted in Dropped().
	DropNewest DropPolicy = iota
	// BlockNewest applies backpressure: producers block until queue
	// space is available. Use only when loss is unacceptable and the
	// caller tolerates added latency.
	BlockNewest
)

// RecorderOptions configures an async EventRecorder. Zero values get
// sane defaults.
type RecorderOptions struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	Workers       int
	Policy        DropPolicy
	Sampler       Sampler
}

// Sampler decides whether an event is kept.
type Sampler interface {
	Keep(e AssignmentEvent) bool
}

// SamplerFunc adapts a function to Sampler.
type SamplerFunc func(AssignmentEvent) bool

func (f SamplerFunc) Keep(e AssignmentEvent) bool { return f(e) }

// AlwaysSampler keeps every event.
var AlwaysSampler Sampler = SamplerFunc(func(AssignmentEvent) bool { return true })

// RatioSampler deterministically keeps approximately rate*100% of
// events. Sampling is a pure function of (experiment,user): the same
// user is consistently included or excluded, which keeps per-user
// metrics stable and avoids sampling flicker across repeat requests.
type RatioSampler struct {
	// permille in [0,1000]; 100 means 10%, 1000 means 100%.
	permille uint32
}

func NewRatioSampler(rate float64) *RatioSampler {
	if rate <= 0 {
		rate = 0
	}
	if rate >= 1 {
		rate = 1
	}
	return &RatioSampler{permille: uint32(rate * 1000)}
}

func (s *RatioSampler) Keep(e AssignmentEvent) bool {
	if s.permille >= 1000 {
		return true
	}
	if s.permille == 0 {
		return false
	}
	return hashUint64("sample:"+e.ExperimentID+"|"+e.UserID)%1000 < uint64(s.permille)
}

const (
	defaultQueueSize     = 4096
	defaultBatchSize     = 256
	defaultFlushInterval = time.Second
	defaultWorkers       = 2
)

type recorderWorker struct {
	r     *EventRecorder
	id    int
	flush chan chan struct{}
}

// EventRecorder asynchronously records assignment events.
//
// Hot-path guarantees:
//   - Record is one sampling check + one non-blocking buffered channel
//     send: no mutex, no disk/network IO on the allocation path.
//   - Memory is bounded by QueueSize regardless of traffic bursts or
//     sink outages; saturation is handled by the configured DropPolicy
//     and exposed via Dropped().
//   - Background workers batch events and flush on size or interval.
type EventRecorder struct {
	sink    EventSink
	opts    RecorderOptions
	sampler Sampler

	queue chan AssignmentEvent
	done  chan struct{}
	wg    sync.WaitGroup

	closed  atomic.Bool
	queued  atomic.Int64 // enqueued after sampling
	dropped atomic.Int64 // queue-full rejections + failed writes
	written atomic.Int64 // successfully delivered to sink

	workerRegMu   sync.Mutex
	workerFlushes []chan chan struct{}
}

// NewEventRecorder validates options and starts background flush workers.
func NewEventRecorder(sink EventSink, opts RecorderOptions) (*EventRecorder, error) {
	if sink == nil {
		return nil, errors.New("split: nil event sink")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = defaultFlushInterval
	}
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.Sampler == nil {
		opts.Sampler = AlwaysSampler
	}
	r := &EventRecorder{
		sink: sink, opts: opts, sampler: opts.Sampler,
		queue: make(chan AssignmentEvent, opts.QueueSize),
		done:  make(chan struct{}),
	}
	for i := 0; i < opts.Workers; i++ {
		w := &recorderWorker{r: r, id: i, flush: make(chan chan struct{}, 1)}
		r.workerRegMu.Lock()
		r.workerFlushes = append(r.workerFlushes, w.flush)
		r.workerRegMu.Unlock()
		r.wg.Add(1)
		go w.run()
	}
	return r, nil
}

// Record enqueues one event. It never performs IO and never panics; a
// closed recorder returns false. Returns true when the event was
// enqueued for delivery.
func (r *EventRecorder) Record(e AssignmentEvent) bool {
	if r.closed.Load() {
		return false
	}
	if !r.sampler.Keep(e) {
		return false
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if r.opts.Policy == BlockNewest {
		select {
		case r.queue <- e:
			r.queued.Add(1)
			return true
		case <-r.done:
			r.dropped.Add(1)
			return false
		}
	}
	select {
	case r.queue <- e:
		r.queued.Add(1)
		return true
	default:
		// Bounded-queue backpressure: count the overflow and move on so
		// a slow downstream can never slow or fail split decisions.
		r.dropped.Add(1)
		return false
	}
}

// Queued reports cumulative events enqueued after sampling.
func (r *EventRecorder) Queued() int64 { return r.queued.Load() }

// Dropped reports cumulative events lost to queue overflow or sink errors.
func (r *EventRecorder) Dropped() int64 { return r.dropped.Load() }

// Written reports cumulative events successfully delivered to the sink.
func (r *EventRecorder) Written() int64 { return r.written.Load() }

func (w *recorderWorker) run() {
	defer w.r.wg.Done()
	buf := make([]AssignmentEvent, 0, w.r.opts.BatchSize)
	ticker := time.NewTicker(w.r.opts.FlushInterval)
	defer ticker.Stop()

	flushBuf := func() {
		if len(buf) == 0 {
			return
		}
		batch := append([]AssignmentEvent(nil), buf...)
		buf = buf[:0]
		w.deliver(batch)
	}

	for {
		select {
		case e := <-w.r.queue:
			buf = append(buf, e)
			if len(buf) >= w.r.opts.BatchSize {
				flushBuf()
			}
		case <-ticker.C:
			flushBuf()
		case ack := <-w.flush:
			flushBuf()
			// Drain any events already queued before the flush request.
			w.drainInto(&buf)
			flushBuf()
			close(ack)
		case <-w.r.done:
			// Drain everything still buffered in the shared queue.
			w.drainInto(&buf)
			flushBuf()
			return
		}
	}
}

func (w *recorderWorker) drainInto(buf *[]AssignmentEvent) {
	for {
		select {
		case e := <-w.r.queue:
			*buf = append(*buf, e)
		default:
			return
		}
	}
}

func (w *recorderWorker) deliver(batch []AssignmentEvent) {
	// Two bounded retries, then drop with a counted loss. Retry
	// backpressure must stay small so a failing sink cannot pin memory.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = w.r.sink.Write(ctx, batch); err == nil {
			w.r.written.Add(int64(len(batch)))
			return
		}
		if attempt < 2 {
			select {
			case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
			case <-w.r.done:
			}
		}
	}
	w.r.dropped.Add(int64(len(batch)))
}

// Flush synchronously flushes all workers' buffered events.
func (r *EventRecorder) Flush(ctx context.Context) error {
	if r.closed.Load() {
		return ErrRecorderClosed
	}
	r.workerRegMu.Lock()
	chs := append([]chan chan struct{}(nil), r.workerFlushes...)
	r.workerRegMu.Unlock()
	// Two fan-out rounds: request every worker to flush+drain first,
	// then once more so events redistributed between workers during the
	// first round are also delivered.
	for round := 0; round < 2; round++ {
		acks := make([]chan struct{}, len(chs))
		for i, ch := range chs {
			acks[i] = make(chan struct{})
			select {
			case ch <- acks[i]:
			case <-ctx.Done():
				return ctx.Err()
			case <-r.done:
				return ErrRecorderClosed
			}
		}
		for _, ack := range acks {
			select {
			case <-ack:
			case <-ctx.Done():
				return ctx.Err()
			case <-r.done:
				return ErrRecorderClosed
			}
		}
	}
	return nil
}

// Close flushes queued events and stops workers. It is idempotent.
func (r *EventRecorder) Close(ctx context.Context) error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(r.done)
	doneAll := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(doneAll)
	}()
	select {
	case <-doneAll:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
