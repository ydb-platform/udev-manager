package mux

import (
	"errors"
	"sync"
)

var errSinkClosed = errors.New("mux: sink is closed")

// elasticSink decouples a producer from a slow consumer with an unbounded
// FIFO queue. Submit appends to the queue and returns immediately; a
// dedicated goroutine drains the queue into the wrapped sink in order.
type elasticSink[T any] struct {
	sink Sink[T]

	mu     sync.Mutex
	queue  []T
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
// Ordering is preserved. Close stops delivery, discards any values still
// queued, and closes the wrapped sink; it never blocks waiting for the
// consumer. Errors returned by the wrapped sink's Submit are reported to
// logger (if non-nil) — they cannot be returned to the producer, which has
// already moved on.
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
		if len(e.queue) == 0 {
			e.mu.Unlock()
			<-e.wake
			continue
		}
		v := e.queue[0]
		e.queue = e.queue[1:]
		if len(e.queue) == 0 {
			e.queue = nil // let the backing array be collected
		}
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
	e.queue = append(e.queue, v)
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
	e.queue = nil
	e.mu.Unlock()

	select {
	case e.wake <- struct{}{}:
	default:
	}
}
