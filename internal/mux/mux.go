// Package mux provides a generic fan-out event multiplexer.
//
// The central abstraction is [Mux], which receives values on a single input
// channel and forwards each value to every registered [Sink]. Sinks can be
// added and removed at runtime without interrupting delivery to other sinks.
//
// Typical usage: create a Mux, subscribe one or more sinks to it, then submit
// values. Each submitted value is delivered to every currently-registered sink.
// Call [Mux.Close] to shut the mux down; it will close all registered sinks.
//
// The package also provides the [Sink] and [Source] interfaces, combinator
// constructors ([ThenSink], [FilterSink]), and synchronisation helpers
// ([AwaitReply], [AwaitDone]) used internally to make Subscribe and
// Unsubscribe synchronous without holding a mutex.
package mux

import (
	"fmt"
	"sync"
	"time"
)

// In is a receive-only channel alias used as a readable event stream.
type In[T any] <-chan T

// Out is a send-only channel alias used as a writable event stream.
type Out[T any] chan<- T

// Logger is a minimal logging interface accepted by [WithLogger].
// Implementations only need Info; it follows a printf-style signature.
type Logger interface {
	Info(format string, args ...interface{})
}

// AwaitReply is a synchronous request–reply helper used to coordinate between
// goroutines without polling.
//
// The sender creates an AwaitReply with [NewAwaitReply], sends it over a
// channel, and calls [AwaitReply.Await] to block until the receiver calls
// [AwaitReply.Reply]. This gives the sender a guarantee that the receiver has
// fully processed the message before execution continues.
//
// T is the request payload type; U is the reply type.
type AwaitReply[T, U any] struct {
	value T
	reply chan U
}

// Value returns the request payload carried by this AwaitReply.
func (ar AwaitReply[T, U]) Value() T {
	return ar.value
}

// Reply sends the response back to the waiting caller and closes the reply
// channel. It must be called exactly once by the receiver.
func (ar AwaitReply[T, U]) Reply(value U) {
	ar.reply <- value
	close(ar.reply)
}

// Await blocks until [AwaitReply.Reply] has been called by the receiver and
// returns the reply value. It must be called exactly once by the sender.
func (ar AwaitReply[T, U]) Await() U {
	return <-ar.reply
}

// AwaitDone is a specialisation of [AwaitReply] for fire-and-wait operations
// that carry no meaningful reply value — the receiver just signals completion.
type AwaitDone[T any] struct {
	AwaitReply[T, struct{}]
}

// Done signals to the waiting caller that the operation is complete.
// It must be called exactly once by the receiver.
func (ad AwaitDone[T]) Done() {
	ad.Reply(struct{}{})
}

// Wait blocks until [AwaitDone.Done] has been called by the receiver.
// It must be called exactly once by the sender.
func (ad AwaitDone[T]) Wait() {
	ad.Await()
}

// NewAwaitReply creates a new AwaitReply carrying the given request value.
func NewAwaitReply[T, U any](value T) AwaitReply[T, U] {
	return AwaitReply[T, U]{
		value: value,
		reply: make(chan U),
	}
}

// NewAwaitDone creates a new AwaitDone carrying the given value.
func NewAwaitDone[T any](value T) AwaitDone[T] {
	return AwaitDone[T]{
		NewAwaitReply[T, struct{}](value),
	}
}

// Sink is the consumer side of an event stream. A Sink receives values via
// [Sink.Submit] and is notified when the stream is finished via [Sink.Close].
//
// Submit must be safe to call from multiple goroutines, but Close will only
// be called once. After Close, Submit must not be called again.
type Sink[T any] interface {
	Submit(T) error
	Close()
}

// thenSink adapts a Sink[T] into a Sink[U] by applying a transformation
// function to each value before forwarding it. This is the contramap pattern:
// the function maps the *input* type, not the output type.
type thenSink[U, T any] struct {
	sink      Sink[T]
	contramap func(U) T
}

func (c *thenSink[U, T]) Submit(v U) error {
	return c.sink.Submit(c.contramap(v))
}

func (c *thenSink[U, T]) Close() {
	c.sink.Close()
}

// ThenSink wraps sink so that each submitted value of type U is first mapped
// to type T by f before being forwarded. The returned Sink[U] delegates Close
// directly to the underlying sink.
func ThenSink[U, T any](sink Sink[T], f func(U) T) Sink[U] {
	return &thenSink[U, T]{sink, f}
}

// filterSink wraps a Sink and only forwards values that pass a predicate.
type filterSink[T any] struct {
	sink Sink[T]
	f    func(T) bool
}

func (c *filterSink[T]) Submit(v T) error {
	if c.f(v) {
		return c.sink.Submit(v)
	}
	return nil
}

func (c *filterSink[T]) Close() {
	c.sink.Close()
}

// FilterSink wraps sink so that only values satisfying f are forwarded.
// Values that do not satisfy f are silently dropped; no error is returned.
func FilterSink[T any](sink Sink[T], f func(T) bool) Sink[T] {
	return &filterSink[T]{sink, f}
}

// chanSink adapts a channel into a Sink. Submit sends to the channel
// (blocking until the receiver is ready); Close closes the channel.
type chanSink[T any] struct {
	ch chan<- T
}

func (c *chanSink[T]) Submit(v T) error {
	c.ch <- v
	return nil
}

func (c *chanSink[T]) Close() {
	close(c.ch)
}

// SinkFromChan wraps a send-only channel as a [Sink]. Submit blocks until the
// channel has capacity; Close closes the underlying channel.
func SinkFromChan[T any](ch chan<- T) Sink[T] {
	return &chanSink[T]{ch}
}

// Source is the producer side of an event stream. Callers subscribe a [Sink]
// and receive a [CancelFunc] that can be used to unsubscribe.
type Source[T any] interface {
	Subscribe(Sink[T]) CancelFunc
}

// Mux is a fan-out multiplexer: values submitted to it are delivered to every
// currently-registered [Sink] in turn.
//
// All public methods are goroutine-safe. Subscribe and Unsubscribe (via the
// returned CancelFunc) are synchronous — they return only after the mux's
// internal goroutine has acknowledged the change, ensuring no value is
// delivered to a sink after its CancelFunc has returned.
//
// A Mux must be created with [Make]; the zero value is not usable.
type Mux[T any] struct {
	// input is the inbound channel. Submit enqueues here; run reads from here.
	input chan T
	// register and unregister carry synchronous add/remove requests to run.
	register   chan AwaitDone[Sink[T]]
	unregister chan AwaitDone[Sink[T]]
	// outputs is the live set of registered sinks, owned exclusively by run.
	outputs map[Sink[T]]bool

	// done is closed by run() on exit, signalling that the mux is shut down.
	// Submit, Subscribe, and CancelFunc select on it to avoid panics.
	done     chan struct{}
	doneOnce sync.Once

	// submitTimeout is how long Submit waits before giving up.
	submitTimeout time.Duration
	// inBufSize is the capacity of the input channel (0 = unbuffered).
	inBufSize int
	logger    Logger
}

// Option is a functional option for [Make].
type Option[T any] func(*Mux[T])

// Buffered returns an [Option] that gives the Mux an input channel with the
// given buffer size. By default the input channel is unbuffered, so Submit
// blocks until the internal goroutine drains it. A buffer allows Submit to
// return immediately when the mux is busy, up to the buffer capacity.
func Buffered[T any](size int) Option[T] {
	return func(m *Mux[T]) { m.inBufSize = size }
}

// WithLogger returns an [Option] that attaches logger to the Mux. The logger
// receives delivery errors (e.g. when a sink's Submit returns an error) and
// submit timeouts.
func WithLogger[T any](logger Logger) Option[T] {
	return func(m *Mux[T]) { m.logger = logger }
}

// Make creates and starts a new Mux. It launches an internal goroutine that
// runs until [Mux.Close] is called.
//
// Optional [Option] values may be passed to configure buffering and logging.
func Make[T any](opts ...Option[T]) *Mux[T] {
	mux := &Mux[T]{
		submitTimeout: 1 * time.Second,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(mux)
		}
	}

	mux.input = make(chan T, mux.inBufSize)
	mux.register = make(chan AwaitDone[Sink[T]])
	mux.unregister = make(chan AwaitDone[Sink[T]])
	mux.outputs = make(map[Sink[T]]bool)
	mux.done = make(chan struct{})

	go mux.run()

	return mux
}

// run is the single goroutine that owns the outputs map and the input channel.
// Using a single owner goroutine means all mutations to outputs are serialised
// without any mutex, and delivery order is well-defined.
//
// Lifecycle:
//   - Runs until Close is called, which closes the register channel.
//   - On exit, closes every remaining sink and signals done.
func (c *Mux[T]) run() {
	defer func() {
		// Signal that the mux is shut down. Submit/Subscribe/CancelFunc
		// select on this to avoid panics on closed channels.
		c.doneOnce.Do(func() { close(c.done) })
	}()
	defer func() {
		// Close all registered sinks so downstream goroutines can terminate.
		for sub := range c.outputs {
			delete(c.outputs, sub)
			sub.Close()
		}
	}()

	for {
		select {
		case v := <-c.input:
			// Fan out: deliver v to every registered sink sequentially.
			// If a sink's Submit returns an error, log it but keep going —
			// one misbehaving sink must not block delivery to others.
			for out := range c.outputs {
				if err := out.Submit(v); err != nil {
					_ = c.error("error submitting value %v: %v", v, err)
				}
			}

		case ar, ok := <-c.register:
			// A closed register channel is the shutdown signal from Close().
			if !ok {
				return
			}
			c.outputs[ar.value] = true
			// Unblock the caller of Subscribe.
			ar.reply <- struct{}{}

		case ar := <-c.unregister:
			sub := ar.value
			delete(c.outputs, sub)
			sub.Close()
			// Unblock the caller of the CancelFunc returned by Subscribe.
			ar.reply <- struct{}{}
		}
	}
}

// error logs the formatted message (if a logger is configured) and also
// returns it as an error for the caller to propagate.
func (m *Mux[T]) error(format string, args ...any) error {
	if m.logger != nil {
		m.logger.Info(format, args...)
	}
	return fmt.Errorf(format, args...)
}

// Close shuts down the Mux. It closes every registered sink and causes the
// internal goroutine to exit. After Close returns, Submit will time out rather
// than block indefinitely.
//
// Close must be called at most once.
func (c *Mux[T]) Close() {
	close(c.register)
}

// Submit enqueues v for delivery to all registered sinks. It blocks until the
// internal goroutine has accepted the value, or until the submit timeout (1 s
// by default) elapses.
//
// Returns an error if the timeout is exceeded; the value is not delivered in
// that case. This prevents a slow or blocked sink from stalling the producer
// indefinitely when a [Buffered] input channel is also full.
func (c *Mux[T]) Submit(v T) error {
	// Fast path: if the mux is already shut down, fail immediately.
	// This prevents values from being enqueued into a buffered input
	// channel after run() has exited (where they would never be delivered).
	select {
	case <-c.done:
		return c.error("mux is closed, cannot submit value %v", v)
	default:
	}

	select {
	case c.input <- v:
		return nil
	case <-c.done:
		return c.error("mux is closed, cannot submit value %v", v)
	case <-time.After(c.submitTimeout):
		return c.error("timed out submitting value %v after %s", v, c.submitTimeout)
	}
}

// CancelFunc is a function that unsubscribes a previously registered [Sink].
// Calling it is synchronous: it returns only after the Mux has confirmed that
// the sink will receive no further values.
type CancelFunc func()

// Subscribe registers sink to receive all values submitted to the Mux.
// It returns a [CancelFunc] that, when called, unregisters the sink and closes
// it. Subscribe is synchronous: it blocks until the internal goroutine has
// acknowledged the registration.
//
// After the CancelFunc returns, no further calls to sink.Submit will be made.
func (c *Mux[T]) Subscribe(sink Sink[T]) CancelFunc {
	ar := NewAwaitDone(sink)
	select {
	case c.register <- ar:
		ar.Wait()
	case <-c.done:
		return func() {}
	}

	return func() {
		ar := NewAwaitDone(sink)
		select {
		case c.unregister <- ar:
			ar.Wait()
		case <-c.done:
			// run() has exited; sink was already closed by run's defer.
		}
	}
}

// ChainCancelFunc returns a single [CancelFunc] that calls cf1, cf2, and any
// additional cfs in order. Nil entries in cfs are skipped.
//
// This is useful for accumulating multiple unsubscribe functions so they can
// all be triggered with a single call, for example on context cancellation.
func ChainCancelFunc(cf1, cf2 func(), cfs ...func()) CancelFunc {
	return func() {
		cf1()
		cf2()
		for _, cf := range cfs {
			if cf != nil {
				cf()
			}
		}
	}
}
