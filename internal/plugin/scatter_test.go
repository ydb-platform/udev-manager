package plugin

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/ydb-platform/udev-manager/internal/udev"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Scatter snapshot projection", func() {
	var (
		matcher *regexp.Regexp
		tmpl    ResourceTemplate
		res     *resource
		scatter *Scatter[*partition]
		updates <-chan struct{}
	)

	BeforeEach(func() {
		matcher = regexp.MustCompile(`nvme_(.*)`)
		tmpl = ResourceTemplate{Domain: "ydb.tech", Prefix: "part-disk01"}
		res = newResource(tmpl, make(map[Id]Instance))
		DeferCleanup(res.Close)

		updates = res.Watch(context.Background())
		Eventually(updates).Should(Receive()) // initial token

		scatter = &Scatter[*partition]{
			templater: PartitionLabelMatcherTemplater("ydb.tech", matcher),
			mapper:    PartitionLabelMatcherInstances("ydb.tech", matcher, false),
			routes:    map[ResourceTemplate]Resource{tmpl: res},
			known:     make(map[ResourceTemplate]map[Id]Instance),
		}
	})

	It("commits a matching device as healthy", func() {
		dev := partitionDevice("nvme0n1p1", "nvme_disk01")
		scatter.applySnapshot(udev.Snapshot{Generation: 1, Devices: []udev.Device{dev}})
		Eventually(updates).Should(Receive())
		Expect(res.Devices()).To(ConsistOf(And(
			HaveField("ID", "disk01"),
			HaveField("Health", "Healthy"),
		)))
	})

	It("retains a removed device as an unhealthy tombstone", func() {
		dev := partitionDevice("nvme0n1p1", "nvme_disk01")
		scatter.applySnapshot(udev.Snapshot{Generation: 1, Devices: []udev.Device{dev}})
		Eventually(updates).Should(Receive())

		scatter.applySnapshot(udev.Snapshot{Generation: 2})
		Eventually(updates).Should(Receive())
		Expect(res.Devices()).To(ConsistOf(And(
			HaveField("ID", "disk01"),
			HaveField("Health", "Unhealthy"),
		)))
	})

	It("does not notify for a snapshot that leaves the resource view unchanged", func() {
		dev := partitionDevice("sda1", "data_01")
		scatter.applySnapshot(udev.Snapshot{Generation: 1, Devices: []udev.Device{dev}})
		Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("refreshes the backing instance for a same-ID replacement", func() {
		oldDev := partitionDevice("same-syspath", "nvme_disk01")
		newDev := partitionDevice("same-syspath", "nvme_disk01")
		newDev.devNode = "/dev/replacement"
		scatter.applySnapshot(udev.Snapshot{Generation: 1, Devices: []udev.Device{oldDev}})
		Eventually(updates).Should(Receive())

		scatter.applySnapshot(udev.Snapshot{Generation: 2, Devices: []udev.Device{newDev}})
		Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
		instance := baseInstance(res.Instances()["disk01"]).(*partition)
		Expect(instance.dev).To(BeIdenticalTo(newDev))
	})

	It("moves a changed same-syspath partition between resource templates", func() {
		oldTemplate := ResourceTemplate{Domain: "ydb.tech", Prefix: "part-old"}
		newTemplate := ResourceTemplate{Domain: "ydb.tech", Prefix: "part-new"}
		oldDev := partitionDevice("same-syspath", "old")
		oldInstance := &partition{label: "old", domain: "ydb.tech", dev: oldDev}
		oldResource := newResource(oldTemplate, map[Id]Instance{oldInstance.Id(): oldInstance})
		newRes := newResource(newTemplate, map[Id]Instance{})
		DeferCleanup(oldResource.Close)
		DeferCleanup(newRes.Close)
		oldUpdates := oldResource.Watch(context.Background())
		newUpdates := newRes.Watch(context.Background())
		Eventually(oldUpdates).Should(Receive())
		Eventually(newUpdates).Should(Receive())

		matcher := regexp.MustCompile(`(.*)`)
		replacementScatter := &Scatter[*partition]{
			templater: PartitionLabelMatcherTemplater("ydb.tech", matcher),
			mapper:    PartitionLabelMatcherInstances("ydb.tech", matcher, false),
			routes: map[ResourceTemplate]Resource{
				oldTemplate: oldResource,
				newTemplate: newRes,
			},
			known: make(map[ResourceTemplate]map[Id]Instance),
		}
		newDev := partitionDevice("same-syspath", "new")
		replacementScatter.applySnapshot(udev.Snapshot{Generation: 2, Devices: []udev.Device{newDev}})

		Eventually(oldUpdates).Should(Receive())
		Eventually(newUpdates).Should(Receive())
		Expect(oldResource.Devices()).To(ConsistOf(And(
			HaveField("ID", "old"),
			HaveField("Health", "Unhealthy"),
		)))
		Expect(newRes.Devices()).To(ConsistOf(And(
			HaveField("ID", "new"),
			HaveField("Health", "Healthy"),
		)))
	})

	It("publishes one resource update for multiple devices in one generation", func() {
		constantTemplate := func(udev.Device) (*ResourceTemplate, error) { return &tmpl, nil }
		mapper := func(dev udev.Device) ([]*partition, error) {
			return []*partition{{label: string(dev.Id()), domain: "ydb.tech", dev: dev}}, nil
		}
		batched := &Scatter[*partition]{
			templater: constantTemplate,
			mapper:    mapper,
			routes:    map[ResourceTemplate]Resource{tmpl: res},
			known:     make(map[ResourceTemplate]map[Id]Instance),
		}
		dev1 := partitionDevice("one", "ignored")
		dev2 := partitionDevice("two", "ignored")
		batched.applySnapshot(udev.Snapshot{Generation: 1, Devices: []udev.Device{dev1, dev2}})

		Eventually(updates).Should(Receive())
		Expect(res.Devices()).To(HaveLen(2))
		Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("does not partially commit a snapshot when mapping fails", func() {
		dev := partitionDevice("nvme0n1p1", "nvme_disk01")
		scatter.applySnapshot(udev.Snapshot{Generation: 1, Devices: []udev.Device{dev}})
		Eventually(updates).Should(Receive())

		failing := &Scatter[*partition]{
			templater: scatter.templater,
			mapper: func(udev.Device) ([]*partition, error) {
				return nil, errors.New("temporary mapping failure")
			},
			routes: scatter.routes,
			known:  scatter.known,
		}
		failing.applySnapshot(udev.Snapshot{Generation: 2, Devices: []udev.Device{dev}})

		Expect(res.Devices()).To(ConsistOf(HaveField("Health", "Healthy")))
		Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("rejects a nil mapped instance without mutating live state", func() {
		dev := partitionDevice("nvme0n1p1", "nvme_disk01")
		scatter.applySnapshot(udev.Snapshot{Generation: 1, Devices: []udev.Device{dev}})
		Eventually(updates).Should(Receive())

		invalid := &Scatter[*partition]{
			templater: scatter.templater,
			mapper: func(udev.Device) ([]*partition, error) {
				return []*partition{nil}, nil
			},
			routes: scatter.routes,
			known:  scatter.known,
		}
		invalid.applySnapshot(udev.Snapshot{Generation: 2, Devices: []udev.Device{dev}})

		Expect(res.Devices()).To(ConsistOf(HaveField("Health", "Healthy")))
		Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
	})
})
