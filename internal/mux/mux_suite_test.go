package mux_test

import (
	"sync"

	"github.com/ydb-platform/udev-manager/internal/mux"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Mux", func() {
	Context("creation", func() {
		It("should create a new Mux without buffer", func() {
			mux := mux.Make[string]()
			Expect(mux).NotTo(BeNil())
			mux.Close()
		})

		It("should create a new Mux with buffer", func() {
			mux := mux.Make(mux.Buffered[string](2))
			Expect(mux).NotTo(BeNil())
			mux.Close()
		})
	})

	Context("registration", func() {
		var m *mux.Mux[string]

		BeforeEach(func() {
			m = mux.Make[string]()
		})

		AfterEach(func() {
			m.Close()
		})

		It("should register a new output channel", func() {
			in := make(chan string)
			cancel := m.Subscribe(mux.SinkFromChan(in))
			Expect(in).NotTo(BeNil())
			Expect(cancel).NotTo(BeNil())
			cancel()
		})

		It("should register a buffered output channel", func() {
			in := make(chan string)
			cancel := m.Subscribe(mux.SinkFromChan(in))
			Expect(in).NotTo(BeNil())
			Expect(cancel).NotTo(BeNil())
			cancel()
		})

		It("should support multiple registrations", func() {
			in1 := make(chan string)
			in2 := make(chan string)
			in3 := make(chan string)
			cancel1 := m.Subscribe(mux.SinkFromChan(in1))
			cancel2 := m.Subscribe(mux.SinkFromChan(in2))
			cancel3 := m.Subscribe(mux.SinkFromChan(in3))

			Expect(in1).NotTo(BeNil())
			Expect(in2).NotTo(BeNil())
			Expect(in3).NotTo(BeNil())

			cancel1()
			cancel2()
			cancel3()
		})

		It("should allow unregistration using cancel function", func() {
			in := make(chan string)
			cancel := m.Subscribe(mux.SinkFromChan(in))
			Expect(in).NotTo(BeNil())

			// Unregister
			cancel()

			// Submit a value
			m.Submit("test")

			// Verify channel doesn't receive the value
			Consistently(in).ShouldNot(Receive())
		})
	})

	Context("submission", func() {
		var m *mux.Mux[string]

		BeforeEach(func() {
			m = mux.Make(mux.WithLogger[string](GinkgoLogr))
		})

		AfterEach(func() {
			m.Close()
		})

		It("should distribute values to all registered outputs", func() {
			in1 := make(chan string)
			in2 := make(chan string)
			cancel1 := m.Subscribe(mux.SinkFromChan(in1))
			cancel2 := m.Subscribe(mux.SinkFromChan(in2))
			defer cancel1()
			defer cancel2()

			go func() {
				m.Submit("hello")
			}()

			Eventually(in1).Should(Receive(Equal("hello")))
			Eventually(in2).Should(Receive(Equal("hello")))
		})

		It("should handle multiple submissions", func() {
			in := make(chan string)
			cancel := m.Subscribe(mux.SinkFromChan(in))
			defer cancel()

			go func() {
				m.Submit("one")
				m.Submit("two")
				m.Submit("three")
			}()

			Eventually(in).Should(Receive(Equal("one")))
			Eventually(in).Should(Receive(Equal("two")))
			Eventually(in).Should(Receive(Equal("three")))

		})
	})

	Context("buffering", func() {
		It("should respect buffer size in RegisterWithBuffer", func() {
			m := mux.Make[int]()
			defer m.Close()

			in := make(chan int, 2)
			cancel := m.Subscribe(mux.SinkFromChan(in))
			defer cancel()

			// These should succeed without blocking
			m.Submit(1)
			m.Submit(2)

			// Verify values
			Eventually(in).Should(Receive(Equal(1)))
			Eventually(in).Should(Receive(Equal(2)))
		})

		It("should respect buffer size in MakeWithBuffer", func() {
			// Create mux with buffer of 2
			m := mux.Make(mux.Buffered[int](2))
			defer m.Close()

			in := make(chan int)
			cancel := m.Subscribe(mux.SinkFromChan(in))
			defer cancel()

			var wg sync.WaitGroup
			wg.Add(1)
			// This goroutine will block after 2 submissions
			go func() {
				defer wg.Done()
				for i := 0; i < 3; i++ {
					m.Submit(i)
				}
			}()

			// Receive values
			Eventually(in).Should(Receive(Equal(0)))
			Eventually(in).Should(Receive(Equal(1)))
			Eventually(in).Should(Receive(Equal(2)))

			wg.Wait()
		})
	})

	Context("closing", func() {
		It("should properly clean up when closed", func() {
			m := mux.Make[string]()
			in1 := make(chan string)
			in2 := make(chan string)
			m.Subscribe(mux.SinkFromChan(in1))
			m.Subscribe(mux.SinkFromChan(in2))

			m.Close()

			// Channels should eventually close
			Eventually(func() bool {
				_, ok := <-in1
				return ok
			}).Should(BeFalse())

			Eventually(func() bool {
				_, ok := <-in2
				return ok
			}).Should(BeFalse())
		})
	})
})
