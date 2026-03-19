package plugin

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("resource cleanup", func() {
	It("closes the instance channel when Close is called before any ListAndWatch", func() {
		r := newResource(
			ResourceTemplate{Domain: "ydb.tech", Prefix: "part-disk01"},
			make(map[Id]Instance),
		)
		r.Close()
		// The run goroutine should exit and close instanceCh.
		Eventually(r.ListAndWatch(context.Background())).Should(BeClosed())
	})
})

var _ = Describe("resource", func() {
	Describe("Name", func() {
		It("returns domain/prefix", func() {
			r := newResource(
				ResourceTemplate{Domain: "ydb.tech", Prefix: "part-disk01"},
				make(map[Id]Instance),
			)
			DeferCleanup(r.Close)
			Expect(r.Name()).To(Equal("ydb.tech/part-disk01"))
		})
	})

	Describe("ListAndWatch", func() {
		var (
			p      *partition
			r      *resource
			ctx    context.Context
			cancel context.CancelFunc
		)

		BeforeEach(func() {
			p = &partition{
				label:  "disk01",
				domain: "ydb.tech",
				dev:    partitionDevice("nvme0n1p1", "disk01"),
			}
			r = newResource(
				ResourceTemplate{Domain: "ydb.tech", Prefix: "part-disk01"},
				map[Id]Instance{p.Id(): p},
			)
			ctx, cancel = context.WithCancel(context.Background())
		})

		AfterEach(func() {
			cancel()
		})

		It("immediately sends the initial set of instances", func() {
			DeferCleanup(r.Close)
			ch := r.ListAndWatch(ctx)
			Eventually(ch).Should(Receive(ContainElement(p)))
		})

		It("forwards instances from subsequent HealthEvents", func() {
			DeferCleanup(r.Close)
			ch := r.ListAndWatch(ctx)
			Eventually(ch).Should(Receive()) // drain the initial send

			r.Submit(HealthEvent{Instances: []Instance{p}, Health: Healthy{}})
			var instances []Instance
			Eventually(ch).Should(Receive(&instances))
			Expect(instances).To(HaveLen(1))
			Expect(instances[0].Id()).To(Equal(p.Id()))
			Expect(instances[0].Health()).To(Equal(Healthy{}))
		})

		It("closes the output channel when resource is closed", func() {
			ch := r.ListAndWatch(ctx)
			Eventually(ch).Should(Receive()) // drain the initial send

			r.Close()
			Eventually(ch).Should(BeClosed())
		})

		It("supports multiple concurrent subscribers", func() {
			DeferCleanup(r.Close)
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()

			ch1 := r.ListAndWatch(ctx)
			ch2 := r.ListAndWatch(ctx2)

			// Both subscribers receive the initial snapshot.
			Eventually(ch1).Should(Receive(ContainElement(p)))
			Eventually(ch2).Should(Receive(ContainElement(p)))

			// Both receive subsequent updates.
			r.Submit(HealthEvent{Instances: []Instance{p}, Health: Unhealthy{}})
			Eventually(ch1).Should(Receive())
			Eventually(ch2).Should(Receive())
		})

		It("cancelling one subscriber does not affect others", func() {
			DeferCleanup(r.Close)
			ctx1, cancel1 := context.WithCancel(context.Background())
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()

			ch1 := r.ListAndWatch(ctx1)
			ch2 := r.ListAndWatch(ctx2)

			Eventually(ch1).Should(Receive()) // drain initial
			Eventually(ch2).Should(Receive()) // drain initial

			// Cancel first subscriber.
			cancel1()
			Eventually(ch1).Should(BeClosed())

			// Second subscriber still receives updates.
			r.Submit(HealthEvent{Instances: []Instance{p}, Health: Healthy{}})
			Eventually(ch2).Should(Receive())
		})

		It("returns a closed channel when resource is already closed", func() {
			r.Close()
			ch := r.ListAndWatch(ctx)
			Eventually(ch).Should(BeClosed())
		})
	})

	Describe("Submit", func() {
		It("updates the instance map", func() {
			r := newResource(
				ResourceTemplate{Domain: "ydb.tech", Prefix: "part-disk01"},
				make(map[Id]Instance),
			)
			DeferCleanup(r.Close)
			p := &partition{label: "disk01", domain: "ydb.tech", dev: partitionDevice("nvme0", "disk01")}
			err := r.Submit(HealthEvent{Instances: []Instance{p}, Health: Healthy{}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Instances()).To(HaveKey(p.Id()))
		})
	})
})
