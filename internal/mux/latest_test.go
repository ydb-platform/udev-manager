package mux_test

import (
	"sync"
	"time"

	"github.com/ydb-platform/udev-manager/internal/mux"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type gatedSink struct {
	started chan int
	release chan struct{}
	out     chan int
	once    sync.Once
}

func newGatedSink() *gatedSink {
	return &gatedSink{
		started: make(chan int),
		release: make(chan struct{}),
		out:     make(chan int, 2),
	}
}

func (s *gatedSink) Submit(value int) error {
	s.started <- value
	<-s.release
	s.out <- value
	return nil
}

func (s *gatedSink) Close() {
	s.once.Do(func() { close(s.out) })
}

var _ = Describe("LatestSink", func() {
	It("keeps only the latest value pending behind an in-flight delivery", func() {
		wrapped := newGatedSink()
		sink := mux.LatestSink[int](wrapped)
		DeferCleanup(func() {
			close(wrapped.release)
			sink.Close()
		})

		Expect(sink.Submit(0)).To(Succeed())
		Eventually(wrapped.started).Should(Receive(Equal(0)))
		for i := 1; i < 10_000; i++ {
			Expect(sink.Submit(i)).To(Succeed())
		}

		wrapped.release <- struct{}{}
		Eventually(wrapped.out).Should(Receive(Equal(0)))
		Eventually(wrapped.started).Should(Receive(Equal(9_999)))
		wrapped.release <- struct{}{}
		Eventually(wrapped.out).Should(Receive(Equal(9_999)))
	})

	It("closes the wrapped sink and rejects later submissions", func() {
		out := make(chan int, 1)
		sink := mux.LatestSink(mux.SinkFromChan(out))
		sink.Close()

		Eventually(out).Should(BeClosed())
		Expect(sink.Submit(1)).NotTo(Succeed())
	})

	It("waits for an in-flight delivery and drops the pending value", func() {
		wrapped := newGatedSink()
		sink := mux.LatestSink[int](wrapped)
		DeferCleanup(func() {
			close(wrapped.release)
			sink.Close()
		})

		Expect(sink.Submit(0)).To(Succeed())
		Eventually(wrapped.started).Should(Receive(Equal(0)))
		Expect(sink.Submit(1)).To(Succeed())

		closed := make(chan struct{})
		go func() {
			sink.Close()
			close(closed)
		}()
		Consistently(closed, 50*time.Millisecond).ShouldNot(BeClosed())

		wrapped.release <- struct{}{}
		Eventually(wrapped.out).Should(Receive(Equal(0)))
		Eventually(closed).Should(BeClosed())
		Eventually(wrapped.out).Should(BeClosed())
	})

	It("is safe to close repeatedly", func() {
		out := make(chan int, 1)
		sink := mux.LatestSink(mux.SinkFromChan(out))
		sink.Close()
		sink.Close()
		Eventually(out).Should(BeClosed())
	})

	It("is safe when submissions race with close", func() {
		out := make(chan int)
		sink := mux.LatestSink(mux.SinkFromChan(out))
		drained := make(chan struct{})
		go func() {
			for range out {
			}
			close(drained)
		}()

		start := make(chan struct{})
		var submitters sync.WaitGroup
		for i := 0; i < 32; i++ {
			submitters.Add(1)
			go func(value int) {
				defer submitters.Done()
				<-start
				_ = sink.Submit(value)
			}(i)
		}
		close(start)
		sink.Close()
		submitters.Wait()
		Eventually(drained).Should(BeClosed())
	})
})
