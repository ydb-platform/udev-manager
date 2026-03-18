// Package mux provides a generic, goroutine-safe fan-out event multiplexer and
// a set of composable filter combinators for working with typed event streams.
//
// # Core model
//
// The central abstraction is [Mux]. A Mux receives values on a single input
// path and delivers each value to every registered [Sink] in turn — a classic
// fan-out (one producer, many consumers). Sinks can be added and removed at
// runtime. Each registration change is synchronous: the call returns only after
// the Mux's internal goroutine has acknowledged the change, so there is never
// a race between subscribing and receiving.
//
// A [Sink] is the consumer side: it receives values via Submit and is notified
// when the stream ends via Close. A [Source] is the producer side: callers
// subscribe a Sink to it and receive a [CancelFunc] to unsubscribe later.
// [Mux] implements both interfaces.
//
// # Getting started
//
// Create a Mux with [Make], submit values with [Mux.Submit], and shut down with
// [Mux.Close]. Every registered Sink will receive every submitted value in the
// order it was submitted.
//
//	m := mux.Make[string]()
//	defer m.Close()
//
//	// Subscribe a simple sink that prints each value.
//	cancel := m.Subscribe(mux.SinkFromChan(myChan))
//	defer cancel()
//
//	m.Submit("hello")
//	m.Submit("world")
//
// # Sinks
//
// A [Sink] has two methods:
//
//   - Submit(v T) error — deliver a value; must be goroutine-safe.
//   - Close() — signal end-of-stream; called at most once, never after Submit.
//
// The simplest way to create a Sink from existing code is [SinkFromChan], which
// wraps any send-only channel:
//
//	ch := make(chan Event, 16)
//	sink := mux.SinkFromChan[Event](ch)
//
// When the Mux (or a CancelFunc) closes the sink, the underlying channel is
// closed, so a goroutine ranging over ch will exit cleanly.
//
// # Subscribing and unsubscribing
//
// [Mux.Subscribe] registers a Sink and returns a [CancelFunc]. Calling the
// CancelFunc unregisters the Sink and closes it. Both operations are
// synchronous:
//
//	cancel := m.Subscribe(sink)
//	// sink is now receiving values.
//
//	cancel()
//	// sink will never receive another value; it has been closed.
//
// The synchronous guarantee means it is safe to free resources associated with
// a Sink immediately after cancel() returns — no delayed delivery can race.
//
// Multiple CancelFuncs can be composed with [ChainCancelFunc]:
//
//	cancel := mux.ChainCancelFunc(cancelA, cancelB, cancelC)
//	cancel() // calls cancelA, cancelB, cancelC in order
//
// This is useful when wiring several sources together; you get a single
// teardown function for the whole group.
//
// # Transforming sinks with ThenSink
//
// [ThenSink] adapts a Sink[T] into a Sink[U] by mapping each incoming value
// before forwarding it. This is the contramap pattern — the function maps the
// input type, not the output type:
//
//	// We have a Sink[int] but the Mux produces strings.
//	intSink := mux.SinkFromChan(intCh)
//	strSink := mux.ThenSink(intSink, func(s string) int {
//		n, _ := strconv.Atoi(s)
//		return n
//	}) // Sink[string] → Sink[int]
//	cancel := stringMux.Subscribe(strSink)
//
// ThenSink does not buffer; the transformation is applied inline inside Submit.
//
// # Filtering sinks with FilterSink
//
// [FilterSink] wraps a Sink so that only values satisfying a predicate are
// forwarded. Values that fail the predicate are silently dropped; no error is
// returned.
//
//	// Only forward even numbers.
//	filtered := mux.FilterSink(intSink, func(n int) bool { return n%2 == 0 })
//	cancel := intMux.Subscribe(filtered)
//
// ThenSink and FilterSink can be chained:
//
//	sink := mux.FilterSink(
//	    mux.ThenSink(intSink, parseEvent),
//	    isImportant,
//	)
//
// # Filtering channels with Filter
//
// [Filter] operates on channels directly instead of Sinks. It reads from an
// input [In] channel, drops values that fail the predicate, and forwards the
// rest to a new [In] channel:
//
//	raw := make(chan Event)
//	// Only pass through Added events.
//	added := mux.Filter[Event](raw, func(e Event) bool {
//	    return e.Kind == EventAdded
//	})
//	for e := range added {
//	    handleAdded(e)
//	}
//
// Filter spawns a goroutine that exits when the input channel is closed. The
// returned channel is closed at that point too, so pipeline stages downstream
// terminate automatically.
//
// # Filter combinators
//
// The package provides several combinators for building [FilterFunc] values.
// They compose cleanly because they all share the same type signature:
// func(T) bool.
//
// [Any] accepts every value; useful as a placeholder or default:
//
//	f := mux.Any[int]() // always returns true
//
// [Not] inverts a predicate:
//
//	isOdd := mux.Not(isEven)
//
// [Or] accepts a value when at least one of the supplied predicates does
// (short-circuit, left-to-right). With no predicates it rejects everything:
//
//	matchAny := mux.Or(isNVMe, isSSD, isHDD)
//
// [And] accepts a value only when all of the supplied predicates do
// (short-circuit, left-to-right). With no predicates it accepts everything:
//
//	matchBoth := mux.And(isBlock, isPartition)
//
// Combinators can be nested arbitrarily:
//
//	f := mux.And(
//	    isBlock,
//	    mux.Or(isNVMe, isSSD),
//	    mux.Not(isRemovable),
//	)
//
// [IsNil] and [IsNotNil] are ready-made FilterFuncs for nilable types. IsNil
// uses reflection so it works correctly for interfaces, pointers, channels,
// maps, slices, and funcs:
//
//	nonNil := mux.Filter(eventCh, mux.IsNotNil[*Device])
//
// # Buffering
//
// By default the Mux's input channel is unbuffered: Submit blocks until the
// internal goroutine is ready to accept the value. Use [Buffered] to add
// capacity so Submit can return immediately when the Mux is busy processing
// the previous value:
//
//	m := mux.Make[Event](mux.Buffered[Event](32))
//
// Note that buffering decouples the producer from the Mux, but delivery to
// each Sink is still sequential and synchronous. A slow Sink will block
// delivery to all Sinks that appear after it in the iteration order. To avoid
// this, give slow Sinks their own buffered channel via [SinkFromChan].
//
// # Logging
//
// Attach a [Logger] to receive delivery errors and submit-timeout messages:
//
//	m := mux.Make[Event](mux.WithLogger[Event](myLogger))
//
// The Logger interface is intentionally minimal:
//
//	type Logger interface {
//	    Info(format string, args ...interface{})
//	}
//
// Any structured logger can satisfy it with a one-line adapter.
//
// # Lifecycle and shutdown
//
// [Mux.Close] shuts the Mux down. It closes every registered Sink and causes
// the internal goroutine to exit. After Close returns, any call to Submit will
// time out (default 1 s) rather than block indefinitely.
//
// Close must be called at most once. A typical pattern using defer:
//
//	m := mux.Make[Event]()
//	defer m.Close()
//
// When Close is called, sinks are closed in an unspecified order. Code that
// owns the sink's underlying channel should wait for the channel to be drained
// or closed before accessing the data.
//
// # Synchronous handshake primitives
//
// [AwaitReply] and [AwaitDone] are low-level helpers used internally to make
// Subscribe and Unsubscribe synchronous without polling or mutexes. They are
// exposed for callers that need the same pattern in their own code.
//
// AwaitReply[T, U] carries a request payload of type T and a reply of type U:
//
//	ar := mux.NewAwaitReply[string, int]("hello")
//	requestCh <- ar     // send the request
//	result := ar.Await() // block until Reply is called
//
// On the receiving side:
//
//	ar := <-requestCh
//	n := process(ar.Value())
//	ar.Reply(n) // unblocks the sender
//
// [AwaitDone] is a specialisation for fire-and-wait operations that carry no
// meaningful reply; the receiver signals completion with Done():
//
//	ad := mux.NewAwaitDone("payload")
//	cmdCh <- ad
//	ad.Wait() // blocks until Done() is called
//
//	// receiver:
//	ad := <-cmdCh
//	doWork(ad.Value())
//	ad.Done()
//
// Both types are single-use: Reply/Done and Await/Wait must each be called
// exactly once.
//
// # Concurrency guarantees
//
// All public methods on [Mux] are goroutine-safe. The Mux's internal goroutine
// is the sole owner of the sink registry; all mutations go through the register
// and unregister channels so no mutex is needed in the hot delivery path.
//
// Delivery within a single Mux is sequential: a value is delivered to all
// sinks before the next value is dequeued. Across multiple Mux instances there
// is no ordering guarantee.
//
// Submit has a default timeout of 1 second. If the Mux's input channel is full
// (or not drained) for that duration, Submit returns an error. Increase
// tolerance by using [Buffered] or by ensuring Sinks process values promptly.
package mux
