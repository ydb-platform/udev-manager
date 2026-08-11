package mux

import (
	"errors"
	"sync"
)

var errLatestSinkClosed = errors.New("mux: latest sink is closed")

// latestSink decouples a producer from a slow consumer while retaining only
// the newest value that has not started delivery. It is suitable for
// level-triggered streams whose values are complete snapshots: an older
// pending snapshot is made obsolete by a newer one.
type latestSink[T any] struct {
	sink Sink[T]

	mu      sync.Mutex
	value   T
	pending bool
	closed  bool
	wake    chan struct{}
	done    chan struct{}
}

// LatestSink wraps sink with a capacity-one, overwrite-latest mailbox.
// Submit never waits for the wrapped sink. If delivery is already in progress,
// one additional latest value is retained; subsequent values replace it.
//
// LatestSink must only be used for complete, authoritative values. It is not
// appropriate for delta/event streams where every value must be observed.
// Close waits for an in-flight wrapped Submit to finish before closing the
// wrapped sink.
func LatestSink[T any](sink Sink[T]) Sink[T] {
	l := &latestSink[T]{
		sink: sink,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	go l.run()
	return l
}

func (l *latestSink[T]) run() {
	defer close(l.done)
	for range l.wake {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			l.sink.Close()
			return
		}
		if !l.pending {
			l.mu.Unlock()
			continue
		}
		value := l.value
		l.pending = false
		l.mu.Unlock()

		_ = l.sink.Submit(value)

		l.mu.Lock()
		pending := l.pending
		closed := l.closed
		l.mu.Unlock()
		if pending || closed {
			l.signal()
		}
	}
}

func (l *latestSink[T]) Submit(value T) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return errLatestSinkClosed
	}
	l.value = value
	l.pending = true
	l.mu.Unlock()
	l.signal()
	return nil
}

func (l *latestSink[T]) Close() {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		l.pending = false
		var zero T
		l.value = zero
	}
	l.mu.Unlock()
	l.signal()

	// A wrapped Submit may already be in progress. Waiting for the delivery
	// worker preserves Sink's close contract: once Close returns, no delivery
	// remains in flight and the wrapped sink has also been closed.
	<-l.done
}

func (l *latestSink[T]) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}
