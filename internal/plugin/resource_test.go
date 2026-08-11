package plugin

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("resource", func() {
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
		r.Close()
	})

	It("returns domain/prefix as its name", func() {
		Expect(r.Name()).To(Equal("ydb.tech/part-disk01"))
	})

	Describe("Watch", func() {
		It("immediately signals the current device view", func() {
			updates := r.Watch(ctx)
			Eventually(updates).Should(Receive())
			Expect(r.Devices()).To(ConsistOf(And(
				HaveField("ID", "disk01"),
				HaveField("Health", "Healthy"),
			)))
		})

		It("serves the latest view when Apply overtakes the initial token", func() {
			updates := r.Watch(ctx)
			Expect(r.Apply(map[Id]Instance{
				p.Id(): instanceWithHealth(p, Unhealthy{}),
			})).To(Succeed())

			Eventually(updates).Should(Receive())
			Expect(r.Devices()).To(ConsistOf(HaveField("Health", "Unhealthy")))
			Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
		})

		It("signals a changed health view", func() {
			updates := r.Watch(ctx)
			Eventually(updates).Should(Receive()) // initial token

			Expect(r.Apply(map[Id]Instance{
				p.Id(): instanceWithHealth(p, Unhealthy{}),
			})).To(Succeed())
			Eventually(updates).Should(Receive())
			Expect(r.Devices()).To(ConsistOf(HaveField("Health", "Unhealthy")))
		})

		It("coalesces a burst to one pending notification and the latest state", func() {
			updates := r.Watch(ctx)
			Eventually(updates).Should(Receive()) // initial token

			for i := 0; i < 10_000; i++ {
				health := Health(Healthy{})
				if i%2 == 0 {
					health = Unhealthy{}
				}
				Expect(r.Apply(map[Id]Instance{
					p.Id(): instanceWithHealth(p, health),
				})).To(Succeed())
			}

			Eventually(updates).Should(Receive())
			Expect(r.Devices()).To(ConsistOf(HaveField("Health", "Healthy")))
			Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
		})

		It("isolates concurrent subscribers", func() {
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()
			updates1 := r.Watch(ctx)
			updates2 := r.Watch(ctx2)
			Eventually(updates1).Should(Receive())
			Eventually(updates2).Should(Receive())

			Expect(r.Apply(map[Id]Instance{
				p.Id(): instanceWithHealth(p, Unhealthy{}),
			})).To(Succeed())
			Eventually(updates1).Should(Receive())
			Eventually(updates2).Should(Receive())
		})

		It("closes only the cancelled subscriber", func() {
			ctx1, cancel1 := context.WithCancel(context.Background())
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()
			updates1 := r.Watch(ctx1)
			updates2 := r.Watch(ctx2)
			Eventually(updates1).Should(Receive())
			Eventually(updates2).Should(Receive())

			cancel1()
			Eventually(updates1).Should(BeClosed())
			Expect(r.Apply(map[Id]Instance{
				p.Id(): instanceWithHealth(p, Unhealthy{}),
			})).To(Succeed())
			Eventually(updates2).Should(Receive())
		})

		It("closes all subscribers when the resource closes", func() {
			updates := r.Watch(ctx)
			Eventually(updates).Should(Receive())
			r.Close()
			Eventually(updates).Should(BeClosed())
		})

		It("returns a closed channel after the resource closes", func() {
			r.Close()
			Eventually(r.Watch(ctx)).Should(BeClosed())
		})
	})

	Describe("Apply", func() {
		It("atomically replaces the instance map", func() {
			p2 := &partition{
				label:  "disk02",
				domain: "ydb.tech",
				dev:    partitionDevice("nvme0n1p2", "disk02"),
			}
			Expect(r.Apply(map[Id]Instance{p2.Id(): p2})).To(Succeed())
			Expect(r.Instances()).To(HaveKey(p2.Id()))
			Expect(r.Instances()).NotTo(HaveKey(p.Id()))
		})

		It("refreshes allocation backing without notifying a visible no-op", func() {
			updates := r.Watch(ctx)
			Eventually(updates).Should(Receive()) // initial token
			replacement := &partition{
				label:  "disk01",
				domain: "ydb.tech",
				dev:    partitionDevice("replacement", "disk01"),
			}

			Expect(r.Apply(map[Id]Instance{replacement.Id(): replacement})).To(Succeed())
			Expect(r.Instances()[replacement.Id()]).To(BeIdenticalTo(replacement))
			Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
		})

		It("rejects updates after close", func() {
			r.Close()
			Expect(r.Apply(map[Id]Instance{})).To(MatchError(errResourceClosed))
		})

		It("protects the frozen view from caller mutation", func() {
			devices := r.Devices()
			devices[0].ID = "mutated"
			devices[0].Health = "mutated"

			Expect(r.Devices()).To(ConsistOf(And(
				HaveField("ID", "disk01"),
				HaveField("Health", "Healthy"),
			)))
		})
	})

})
