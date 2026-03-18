package mux_test

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ydb-platform/udev-manager/internal/mux"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// collectSink is a thread-safe Sink that records every submitted value and
// whether Close has been called. It is used throughout the tests below as a
// controllable consumer that doesn't require a channel.
type collectSink[T any] struct {
	mu     sync.Mutex
	values []T
	closed bool
}

func (s *collectSink[T]) Submit(v T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = append(s.values, v)
	return nil
}

func (s *collectSink[T]) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// Values returns a snapshot of the collected values so far.
func (s *collectSink[T]) Values() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]T, len(s.values))
	copy(out, s.values)
	return out
}

// Closed returns whether Close has been called.
func (s *collectSink[T]) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// slowSink is a Sink that sleeps on each Submit, simulating a slow consumer.
type slowSink[T any] struct {
	mu     sync.Mutex
	values []T
	closed bool
	delay  time.Duration
}

func (s *slowSink[T]) Submit(v T) error {
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = append(s.values, v)
	return nil
}

func (s *slowSink[T]) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

func (s *slowSink[T]) Values() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]T, len(s.values))
	copy(out, s.values)
	return out
}

// errSink is a Sink whose Submit always returns a fixed error.
// It is used to verify that the Mux logs sink errors via WithLogger.
type errSink[T any] struct{ err error }

func (s *errSink[T]) Submit(T) error { return s.err }
func (s *errSink[T]) Close()         {}

// testLogger captures Info calls for inspection in tests.
type testLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *testLogger) Info(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, fmt.Sprintf(format, args...))
}

func (l *testLogger) Messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.msgs))
	copy(out, l.msgs)
	return out
}

// ---------------------------------------------------------------------------
// Any
// ---------------------------------------------------------------------------

var _ = Describe("Any", func() {
	It("accepts every value regardless of content", func() {
		f := mux.Any[int]()
		Expect(f(0)).To(BeTrue())
		Expect(f(-1)).To(BeTrue())
		Expect(f(42)).To(BeTrue())
	})

	It("works with non-numeric types", func() {
		f := mux.Any[string]()
		Expect(f("")).To(BeTrue())
		Expect(f("hello")).To(BeTrue())
	})
})

// ---------------------------------------------------------------------------
// ThenSink
// ---------------------------------------------------------------------------

var _ = Describe("ThenSink", func() {
	It("maps each value through the function before forwarding", func() {
		inner := &collectSink[int]{}
		sink := mux.ThenSink(inner, func(s string) int { return len(s) })

		Expect(sink.Submit("hello")).To(Succeed())
		Expect(sink.Submit("hi")).To(Succeed())
		Expect(sink.Submit("")).To(Succeed())

		Expect(inner.Values()).To(Equal([]int{5, 2, 0}))
	})

	It("propagates errors returned by the inner sink's Submit", func() {
		boom := errors.New("inner error")
		inner := &errSink[int]{err: boom}
		sink := mux.ThenSink(inner, func(s string) int { return len(s) })

		Expect(sink.Submit("anything")).To(MatchError(boom))
	})

	It("delegates Close to the inner sink", func() {
		inner := &collectSink[int]{}
		sink := mux.ThenSink(inner, func(s string) int { return len(s) })

		sink.Close()

		Expect(inner.Closed()).To(BeTrue())
	})

	It("can be chained with another ThenSink", func() {
		// string → int → bool
		inner := &collectSink[bool]{}
		intSink := mux.ThenSink(inner, func(n int) bool { return n > 3 })
		strSink := mux.ThenSink(intSink, func(s string) int { return len(s) })

		strSink.Submit("hi")    // len=2 → false
		strSink.Submit("hello") // len=5 → true

		Expect(inner.Values()).To(Equal([]bool{false, true}))
	})
})

// ---------------------------------------------------------------------------
// FilterSink
// ---------------------------------------------------------------------------

var _ = Describe("FilterSink", func() {
	It("forwards values that satisfy the predicate", func() {
		inner := &collectSink[int]{}
		sink := mux.FilterSink(inner, func(n int) bool { return n%2 == 0 })

		sink.Submit(1)
		sink.Submit(2)
		sink.Submit(3)
		sink.Submit(4)

		Expect(inner.Values()).To(Equal([]int{2, 4}))
	})

	It("returns nil (no error) for dropped values", func() {
		inner := &collectSink[int]{}
		sink := mux.FilterSink(inner, func(int) bool { return false })

		Expect(sink.Submit(99)).To(Succeed())
		Expect(inner.Values()).To(BeEmpty())
	})

	It("delegates Close to the inner sink", func() {
		inner := &collectSink[int]{}
		sink := mux.FilterSink(inner, mux.Any[int]())

		sink.Close()

		Expect(inner.Closed()).To(BeTrue())
	})

	It("can be composed with ThenSink", func() {
		// ThenSink converts string → int; FilterSink drops negatives.
		inner := &collectSink[int]{}
		filtered := mux.FilterSink(inner, func(n int) bool { return n >= 0 })
		mapped := mux.ThenSink(filtered, func(n int) int { return n - 3 })

		mapped.Submit(1) // 1-3=-2 → dropped
		mapped.Submit(5) // 5-3=2  → kept

		Expect(inner.Values()).To(Equal([]int{2}))
	})
})

// ---------------------------------------------------------------------------
// ChainCancelFunc
// ---------------------------------------------------------------------------

var _ = Describe("ChainCancelFunc", func() {
	It("calls both required functions in order", func() {
		var calls []int
		c1 := func() { calls = append(calls, 1) }
		c2 := func() { calls = append(calls, 2) }

		cancel := mux.ChainCancelFunc(c1, c2)
		cancel()

		Expect(calls).To(Equal([]int{1, 2}))
	})

	It("calls variadic functions after the two required ones", func() {
		var calls []int
		fn := func(n int) func() { return func() { calls = append(calls, n) } }

		cancel := mux.ChainCancelFunc(fn(1), fn(2), fn(3), fn(4))
		cancel()

		Expect(calls).To(Equal([]int{1, 2, 3, 4}))
	})

	It("skips nil entries in the variadic tail", func() {
		var calls []int
		c1 := func() { calls = append(calls, 1) }
		c2 := func() { calls = append(calls, 2) }

		cancel := mux.ChainCancelFunc(c1, c2, nil, nil)
		cancel()

		Expect(calls).To(Equal([]int{1, 2}))
	})

	It("can be nested to build a chain incrementally", func() {
		var calls []int
		fn := func(n int) func() { return func() { calls = append(calls, n) } }

		// Simulate the pattern used in main.go: accumulate cancels in a loop.
		cancel := mux.CancelFunc(fn(1))
		cancel = mux.ChainCancelFunc(fn(2), cancel)
		cancel = mux.ChainCancelFunc(fn(3), cancel)
		cancel()

		Expect(calls).To(Equal([]int{3, 2, 1}))
	})
})

// ---------------------------------------------------------------------------
// AwaitReply
// ---------------------------------------------------------------------------

var _ = Describe("AwaitReply", func() {
	It("carries the request value via Value()", func() {
		ar := mux.NewAwaitReply[string, int]("the-request")
		Expect(ar.Value()).To(Equal("the-request"))
	})

	It("Await returns the value passed to Reply", func() {
		ar := mux.NewAwaitReply[string, int]("req")
		go ar.Reply(42)
		Expect(ar.Await()).To(Equal(42))
	})

	It("unblocks Await exactly when Reply is called", func() {
		ar := mux.NewAwaitReply[int, string](99)
		arrived := make(chan string, 1)

		go func() {
			arrived <- ar.Await()
		}()

		// Nothing should have arrived before Reply.
		Consistently(arrived, "20ms").ShouldNot(Receive())

		ar.Reply("reply-value")

		Eventually(arrived).Should(Receive(Equal("reply-value")))
	})
})

// ---------------------------------------------------------------------------
// AwaitDone
// ---------------------------------------------------------------------------

var _ = Describe("AwaitDone", func() {
	It("carries the payload via Value()", func() {
		ad := mux.NewAwaitDone("payload")
		Expect(ad.Value()).To(Equal("payload"))
	})

	It("Wait returns after Done is called", func() {
		ad := mux.NewAwaitDone("work")
		waited := make(chan struct{})

		go func() {
			ad.Wait()
			close(waited)
		}()

		Consistently(waited, "20ms").ShouldNot(BeClosed())

		ad.Done()

		Eventually(waited).Should(BeClosed())
	})
})

// ---------------------------------------------------------------------------
// Mux — Subscribe and CancelFunc synchrony
// ---------------------------------------------------------------------------

var _ = Describe("Mux synchrony", func() {
	var m *mux.Mux[int]

	BeforeEach(func() {
		m = mux.Make[int]()
	})

	AfterEach(func() {
		m.Close()
	})

	Describe("Subscribe", func() {
		It("guarantees that values submitted after Subscribe returns are delivered", func() {
			sink := &collectSink[int]{}
			cancel := m.Subscribe(sink)
			defer cancel()

			// Subscribe is synchronous, so any Submit issued afterwards is
			// guaranteed to reach this sink.
			go m.Submit(1)

			Eventually(sink.Values).Should(ConsistOf(1))
		})
	})

	Describe("CancelFunc", func() {
		It("is synchronous: no further deliveries occur after it returns", func() {
			sink := &collectSink[int]{}
			cancel := m.Subscribe(sink)

			// Deliver one value and confirm it arrives.
			go m.Submit(1)
			Eventually(sink.Values).Should(ConsistOf(1))

			// Unsubscribe synchronously.
			cancel()

			// Use a second sink as a barrier: once it receives 2 we know the
			// Mux's delivery pass for that value is complete, and our cancelled
			// sink must not have received it.
			barrierCh := make(chan int, 1)
			barrierCancel := m.Subscribe(mux.SinkFromChan(barrierCh))
			defer barrierCancel()

			go m.Submit(2)
			Eventually(barrierCh).Should(Receive(Equal(2)))

			Expect(sink.Values()).To(ConsistOf(1))
		})

		It("closes the sink when called", func() {
			sink := &collectSink[int]{}
			cancel := m.Subscribe(sink)
			cancel()
			Expect(sink.Closed()).To(BeTrue())
		})

		It("does not affect other subscribed sinks", func() {
			cancelled := &collectSink[int]{}
			kept := &collectSink[int]{}

			cancelFirst := m.Subscribe(cancelled)
			keepSecond := m.Subscribe(kept)
			defer keepSecond()

			cancelFirst()

			go m.Submit(7)
			Eventually(kept.Values).Should(ConsistOf(7))
			Expect(cancelled.Values()).To(BeEmpty())
		})
	})
})

// Close tests live outside the shared BeforeEach/AfterEach above because the
// test itself calls m.Close(); a second Close in AfterEach would panic.
var _ = Describe("Mux Close", func() {
	It("closes all registered sinks", func() {
		m := mux.Make[int]()
		s1, s2 := &collectSink[int]{}, &collectSink[int]{}
		m.Subscribe(s1)
		m.Subscribe(s2)

		m.Close()

		Eventually(s1.Closed).Should(BeTrue())
		Eventually(s2.Closed).Should(BeTrue())
	})

	It("is idempotent and does not panic on double close", func() {
		m := mux.Make[int]()
		sink := &collectSink[int]{}
		m.Subscribe(sink)

		m.Close()
		Expect(func() { m.Close() }).NotTo(Panic())
		Eventually(sink.Closed).Should(BeTrue())
	})

	It("completes shutdown even when a sink is slow", func() {
		m := mux.Make[int](mux.Buffered[int](4))
		slow := &slowSink[int]{delay: 50 * time.Millisecond}
		m.Subscribe(slow)

		// Fill the buffer with a few values.
		for i := 0; i < 3; i++ {
			_ = m.Submit(i)
		}

		// Wait for all values to be delivered before closing.
		Eventually(func() int { return len(slow.Values()) }, "2s").Should(Equal(3))

		// Close should complete in a bounded time despite the slow sink.
		done := make(chan struct{})
		go func() {
			m.Close()
			close(done)
		}()
		Eventually(done, "2s").Should(BeClosed())
	})

	It("drains buffered values to sinks before closing them", func() {
		m := mux.Make[int](mux.Buffered[int](8))
		sink := &collectSink[int]{}
		m.Subscribe(sink)

		// Submit several values.
		for i := 0; i < 5; i++ {
			_ = m.Submit(i)
		}
		// Give the mux goroutine time to deliver some.
		time.Sleep(20 * time.Millisecond)

		m.Close()

		// All 5 values must have been delivered.
		Eventually(sink.Closed).Should(BeTrue())
		Expect(sink.Values()).To(HaveLen(5))
	})
})

// ---------------------------------------------------------------------------
// WithLogger — sink errors are logged
// ---------------------------------------------------------------------------

var _ = Describe("WithLogger", func() {
	It("logs an error when a sink's Submit returns one", func() {
		logger := &testLogger{}
		m := mux.Make[int](mux.WithLogger[int](logger))
		defer m.Close()

		m.Subscribe(&errSink[int]{err: errors.New("sink-boom")})

		go m.Submit(1)

		Eventually(logger.Messages).Should(
			ContainElement(ContainSubstring("sink-boom")),
		)
	})

	It("continues delivering to other sinks when one errors", func() {
		logger := &testLogger{}
		m := mux.Make[int](mux.WithLogger[int](logger))
		defer m.Close()

		good := &collectSink[int]{}
		m.Subscribe(&errSink[int]{err: errors.New("bad")})
		m.Subscribe(good)

		go m.Submit(42)

		// The good sink must still receive the value despite the bad sink erroring.
		Eventually(good.Values).Should(ConsistOf(42))
	})
})
