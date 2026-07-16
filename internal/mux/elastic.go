package mux

import (
	"errors"
	"sync"
)

var errSinkClosed = errors.New("mux: sink is closed")

const (
	initialElasticQueueCapacity     = 16
	maxRetainedElasticQueueCapacity = 1024
)

// ringQueue is a growable FIFO backed by a circular buffer. Unlike repeatedly
// reslicing from the front, it reuses slots released by Pop and only copies
// elements when the buffer has to grow.
type ringQueue[T any] struct {
	values []T
	head   int
	count  int
}

func (q *ringQueue[T]) len() int {
	return q.count
}

func (q *ringQueue[T]) push(v T) {
	if q.count == len(q.values) {
		q.grow()
	}

	tail := q.head + q.count
	if tail >= len(q.values) {
		tail -= len(q.values)
	}
	q.values[tail] = v
	q.count++
}

func (q *ringQueue[T]) pop() (T, bool) {
	if q.count == 0 {
		var zero T
		return zero, false
	}

	v := q.values[q.head]
	var zero T
	q.values[q.head] = zero
	q.head++
	if q.head == len(q.values) {
		q.head = 0
	}
	q.count--

	if q.count == 0 {
		q.head = 0
		// Reuse normal burst capacity, but do not retain a pathological peak
		// forever after the consumer catches up.
		if len(q.values) > maxRetainedElasticQueueCapacity {
			q.values = nil
		}
	}

	return v, true
}

func (q *ringQueue[T]) clear() {
	q.values = nil
	q.head = 0
	q.count = 0
}

func (q *ringQueue[T]) grow() {
	capacity := len(q.values) * 2
	if capacity < initialElasticQueueCapacity {
		capacity = initialElasticQueueCapacity
	}

	values := make([]T, capacity)
	if q.count > 0 {
		if q.head+q.count <= len(q.values) {
			copy(values, q.values[q.head:q.head+q.count])
		} else {
			n := copy(values, q.values[q.head:])
			copy(values[n:], q.values[:q.count-n])
		}
	}
	q.values = values
	q.head = 0
}

// elasticSink decouples a producer from a slow consumer with an unbounded
// FIFO queue. Submit appends to the queue and returns immediately; a
// dedicated goroutine drains the queue into the wrapped sink in order.
type elasticSink[T any] struct {
	sink Sink[T]

	mu     sync.Mutex
	queue  ringQueue[T]
	closed bool
	wake   chan struct{}
	logger Logger
}

// ElasticSink wraps sink with an unbounded FIFO queue so that Submit never
// blocks, regardless of how slowly the wrapped sink consumes values.
//
// This protects fan-out producers (e.g. a udev event monitor) from
// back-pressure: a subscriber that is busy for a long time cannot stall the
// producer and cause event loss upstream. The cost is unbounded memory in
// the pathological case of a consumer that never drains; callers should only
// use this for consumers that are guaranteed to make progress.
//
// Ordering is preserved. Close stops accepting new values and discards any
// values still queued. The wrapped sink is closed once the drain goroutine
// observes the close; if the wrapped sink's Submit can block indefinitely,
// close propagation may be delayed. Errors from the wrapped sink's Submit are
// reported to logger (if non-nil) — they cannot be returned to the producer.
func ElasticSink[T any](sink Sink[T], logger Logger) Sink[T] {
	e := &elasticSink[T]{
		sink:   sink,
		wake:   make(chan struct{}, 1),
		logger: logger,
	}
	go e.run()
	return e
}

func (e *elasticSink[T]) run() {
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			e.sink.Close()
			return
		}
		if e.queue.len() == 0 {
			e.mu.Unlock()
			<-e.wake
			continue
		}
		v, _ := e.queue.pop()
		e.mu.Unlock()

		if err := e.sink.Submit(v); err != nil && e.logger != nil {
			e.logger.Info("elastic sink: failed to deliver value %v: %v", v, err)
		}
	}
}

func (e *elasticSink[T]) Submit(v T) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errSinkClosed
	}
	e.queue.push(v)
	e.mu.Unlock()

	select {
	case e.wake <- struct{}{}:
	default:
	}
	return nil
}

func (e *elasticSink[T]) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.queue.clear()
	e.mu.Unlock()

	select {
	case e.wake <- struct{}{}:
	default:
	}
}
