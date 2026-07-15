package mux_test

import (
	"time"

	"github.com/ydb-platform/udev-manager/internal/mux"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ElasticSink", func() {
	It("delivers values to the wrapped sink in order", func() {
		out := make(chan int, 10)
		sink := mux.ElasticSink(mux.SinkFromChan(out), nil)
		defer sink.Close()

		for i := 0; i < 10; i++ {
			Expect(sink.Submit(i)).To(Succeed())
		}

		for i := 0; i < 10; i++ {
			Eventually(out).Should(Receive(Equal(i)))
		}
	})

	It("does not block Submit when the consumer is slow", func() {
		out := make(chan int) // unbuffered, nobody reading yet
		sink := mux.ElasticSink(mux.SinkFromChan(out), nil)
		defer sink.Close()

		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			for i := 0; i < 1000; i++ {
				Expect(sink.Submit(i)).To(Succeed())
			}
		}()

		// All 1000 submissions must complete without a single read.
		Eventually(done).Should(BeClosed())

		// And the values are still delivered in order once we start reading.
		for i := 0; i < 1000; i++ {
			Eventually(out).Should(Receive(Equal(i)))
		}
	})

	It("closes the wrapped sink on Close", func() {
		out := make(chan int)
		sink := mux.ElasticSink(mux.SinkFromChan(out), nil)

		sink.Close()

		Eventually(out).Should(BeClosed())
	})

	It("rejects Submit after Close", func() {
		out := make(chan int)
		sink := mux.ElasticSink(mux.SinkFromChan(out), nil)

		sink.Close()

		Expect(sink.Submit(1)).NotTo(Succeed())
	})

	It("is safe to Close twice", func() {
		out := make(chan int)
		sink := mux.ElasticSink(mux.SinkFromChan(out), nil)

		sink.Close()
		sink.Close()

		Eventually(out).Should(BeClosed())
	})

	It("does not block Close on undelivered values", func() {
		out := make(chan int) // unbuffered, never read
		sink := mux.ElasticSink(mux.SinkFromChan(out), nil)

		Expect(sink.Submit(1)).To(Succeed())
		Expect(sink.Submit(2)).To(Succeed())

		closed := make(chan struct{})
		go func() {
			defer close(closed)
			sink.Close()
		}()
		Eventually(closed, time.Second).Should(BeClosed())
	})
})
