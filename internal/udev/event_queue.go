package udev

import (
	"fmt"
	"sync"

	"k8s.io/klog/v2"

	"github.com/ydb-platform/udev-manager/internal/mux"
)

// eventQueue keeps discovery delivery independent of subscriber progress.
// Every delta must be retained: dropping a Removed event leaves stale devices
// in the subscriber. The queue is unbounded, so a stalled subscriber consumes
// memory until it resumes. It must eventually consume events for Close to finish.
type eventQueue struct {
	sink    mux.Sink[Event]
	mu      sync.Mutex
	ready   *sync.Cond
	pending []Event
	closed  bool
	done    chan struct{}
}

func newEventQueue(sink mux.Sink[Event]) *eventQueue {
	q := &eventQueue{sink: sink, done: make(chan struct{})}
	q.ready = sync.NewCond(&q.mu)
	go q.run()
	return q
}

func (q *eventQueue) Submit(ev Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return fmt.Errorf("udev: event queue is closed")
	}
	q.pending = append(q.pending, ev)
	q.ready.Signal()
	return nil
}

// Close drains accepted events and closes the subscriber. On return, there
// are no deliveries in flight. Repeated calls are safe.
func (q *eventQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.ready.Signal()
	q.mu.Unlock()
	<-q.done
}

func (q *eventQueue) run() {
	defer close(q.done)
	defer q.sink.Close()
	for {
		q.mu.Lock()
		for len(q.pending) == 0 && !q.closed {
			q.ready.Wait()
		}
		if len(q.pending) == 0 {
			q.mu.Unlock()
			return
		}
		ev := q.pending[0]
		q.pending[0] = nil
		q.pending = q.pending[1:]
		if len(q.pending) == 0 {
			q.pending = nil
		}
		q.mu.Unlock()

		if err := q.sink.Submit(ev); err != nil {
			klog.Errorf("udev: failed to deliver queued event: %v", err)
		}
	}
}
