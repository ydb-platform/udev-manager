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

	It("isolates a mapper failure to its resource template", func() {
		const (
			templateProperty = "TEST_TEMPLATE"
			instanceProperty = "TEST_INSTANCE"
			failProperty     = "TEST_FAIL"
		)
		templateA := ResourceTemplate{Domain: "ydb.tech", Prefix: "part-a"}
		templateB := ResourceTemplate{Domain: "ydb.tech", Prefix: "part-b"}
		resourceA := newResource(templateA, map[Id]Instance{})
		resourceB := newResource(templateB, map[Id]Instance{})
		DeferCleanup(resourceA.Close)
		DeferCleanup(resourceB.Close)
		updatesA := resourceA.Watch(context.Background())
		updatesB := resourceB.Watch(context.Background())
		Eventually(updatesA).Should(Receive())
		Eventually(updatesB).Should(Receive())

		device := func(syspath, route, instance string, fail bool) *mockDevice {
			dev := partitionDevice(syspath, "unused")
			dev.properties[templateProperty] = route
			dev.properties[instanceProperty] = instance
			if fail {
				dev.properties[failProperty] = "true"
			}
			return dev
		}
		templater := func(dev udev.Device) (*ResourceTemplate, error) {
			return &ResourceTemplate{
				Domain: "ydb.tech",
				Prefix: "part-" + dev.Property(templateProperty),
			}, nil
		}
		mapper := func(dev udev.Device) ([]*partition, error) {
			if dev.Property(failProperty) != "" {
				return nil, errors.New("temporary mapping failure")
			}
			return []*partition{{
				domain: "ydb.tech",
				label:  dev.Property(instanceProperty),
				dev:    dev,
			}}, nil
		}
		isolated := &Scatter[*partition]{
			templater: templater,
			mapper:    mapper,
			routes: map[ResourceTemplate]Resource{
				templateA: resourceA,
				templateB: resourceB,
			},
			known: make(map[ResourceTemplate]map[Id]Instance),
		}

		oldA := device("device-a", "a", "a-old", false)
		oldB1 := device("device-b1", "b", "b-one", false)
		oldB2 := device("device-b2", "b", "b-two", false)
		isolated.applySnapshot(udev.Snapshot{
			Generation: 1,
			Devices:    []udev.Device{oldA, oldB1, oldB2},
		})
		Eventually(updatesA).Should(Receive())
		Eventually(updatesB).Should(Receive())

		newA := device("device-a", "a", "a-new", false)
		failedB1 := device("device-b1", "b", "b-one", true)
		newB2 := device("device-b2", "b", "b-new", false)
		isolated.applySnapshot(udev.Snapshot{
			Generation: 2,
			Devices:    []udev.Device{newA, failedB1, newB2},
		})

		Eventually(updatesA).Should(Receive())
		Expect(resourceA.Devices()).To(ConsistOf(
			And(HaveField("ID", "a-old"), HaveField("Health", "Unhealthy")),
			And(HaveField("ID", "a-new"), HaveField("Health", "Healthy")),
		))
		Expect(resourceB.Devices()).To(ConsistOf(
			And(HaveField("ID", "b-one"), HaveField("Health", "Healthy")),
			And(HaveField("ID", "b-two"), HaveField("Health", "Healthy")),
		))
		Expect(baseInstance(resourceB.Instances()["b-two"]).(*partition).dev).To(BeIdenticalTo(oldB2))
		Consistently(updatesB, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("uses the previous device route to isolate a templater failure", func() {
		templateA := ResourceTemplate{Domain: "ydb.tech", Prefix: "part-a"}
		templateB := ResourceTemplate{Domain: "ydb.tech", Prefix: "part-b"}
		resourceA := newResource(templateA, map[Id]Instance{})
		resourceB := newResource(templateB, map[Id]Instance{})
		DeferCleanup(resourceA.Close)
		DeferCleanup(resourceB.Close)

		templater := func(dev udev.Device) (*ResourceTemplate, error) {
			if dev.Property("TEST_FAIL") != "" {
				return nil, errors.New("temporary templating failure")
			}
			return &ResourceTemplate{Domain: "ydb.tech", Prefix: "part-" + dev.Property("TEST_TEMPLATE")}, nil
		}
		mapper := func(dev udev.Device) ([]*partition, error) {
			return []*partition{{domain: "ydb.tech", label: dev.Property("TEST_INSTANCE"), dev: dev}}, nil
		}
		isolated := &Scatter[*partition]{
			templater: templater,
			mapper:    mapper,
			routes: map[ResourceTemplate]Resource{
				templateA: resourceA,
				templateB: resourceB,
			},
			known: make(map[ResourceTemplate]map[Id]Instance),
		}
		device := func(syspath, route, instance string, fail bool) *mockDevice {
			dev := partitionDevice(syspath, "unused")
			dev.properties["TEST_TEMPLATE"] = route
			dev.properties["TEST_INSTANCE"] = instance
			if fail {
				dev.properties["TEST_FAIL"] = "true"
			}
			return dev
		}

		oldA := device("device-a", "a", "a-old", false)
		oldB := device("device-b", "b", "b-old", false)
		isolated.applySnapshot(udev.Snapshot{Generation: 1, Devices: []udev.Device{oldA, oldB}})

		newA := device("device-a", "a", "a-new", false)
		failedB := device("device-b", "b", "ignored", true)
		isolated.applySnapshot(udev.Snapshot{Generation: 2, Devices: []udev.Device{newA, failedB}})

		Expect(resourceA.Devices()).To(ContainElement(And(
			HaveField("ID", "a-new"),
			HaveField("Health", "Healthy"),
		)))
		Expect(resourceB.Devices()).To(ConsistOf(And(
			HaveField("ID", "b-old"),
			HaveField("Health", "Healthy"),
		)))
		Expect(baseInstance(resourceB.Instances()["b-old"]).(*partition).dev).To(BeIdenticalTo(oldB))
	})

	It("preserves every resource when a failure cannot be attributed", func() {
		dev := partitionDevice("nvme0n1p1", "nvme_disk01")
		scatter.applySnapshot(udev.Snapshot{Generation: 1, Devices: []udev.Device{dev}})
		Eventually(updates).Should(Receive())

		scatter.applySnapshot(udev.Snapshot{Generation: 2, Devices: []udev.Device{nil}})

		Expect(res.Devices()).To(ConsistOf(And(
			HaveField("ID", "disk01"),
			HaveField("Health", "Healthy"),
		)))
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
