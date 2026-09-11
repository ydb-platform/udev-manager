package plugin

import (
	"context"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ydb-platform/udev-manager/internal/udev"
)

var _ = Describe("Scatter", func() {
	var (
		matcher *regexp.Regexp
		tmpl    ResourceTemplate
		res     *resource
		scatter *Scatter[*partition]
		watchCh <-chan []Instance
	)

	BeforeEach(func() {
		matcher = regexp.MustCompile(`nvme_(.*)`)
		tmpl = ResourceTemplate{Domain: "ydb.tech", Prefix: "part-disk01"}
		res = newResource(tmpl, make(map[Id]Instance))
		DeferCleanup(res.Close)

		watchCh = res.ListAndWatch(context.Background())
		Eventually(watchCh).Should(Receive()) // drain initial snapshot

		scatter = &Scatter[*partition]{
			templater: PartitionLabelMatcherTemplater("ydb.tech", matcher),
			mapper:    PartitionLabelMatcherInstances("ydb.tech", matcher, false),
			registry:  nil, // not exercised when the route already exists
			routes:    map[ResourceTemplate]Resource{tmpl: res},
		}
	})

	Describe("added with an existing route", func() {
		It("submits a HealthEvent to the existing resource", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_disk01")
			scatter.added(dev)
			Eventually(watchCh).Should(Receive())
		})

		It("the submitted event carries the matched instance", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_disk01")
			scatter.added(dev)
			var instances []Instance
			Eventually(watchCh).Should(Receive(&instances))
			Expect(instances).To(HaveLen(1))
		})

		It("does nothing for a non-matching device", func() {
			dev := partitionDevice("sda1", "data_01")
			scatter.added(dev)
			Consistently(watchCh, 50*time.Millisecond).ShouldNot(Receive())
		})
	})

	Describe("added without instances", func() {
		It("does not register a resource for an empty mapper result", func() {
			emptyTemplate := ResourceTemplate{Domain: "ydb.tech", Prefix: "netrdma-0"}
			emptyScatter := &Scatter[*partition]{
				templater: func(udev.Device) (*ResourceTemplate, error) {
					return &emptyTemplate, nil
				},
				mapper: func(udev.Device) ([]*partition, error) {
					return nil, nil
				},
				registry: nil,
				routes:   make(map[ResourceTemplate]Resource),
			}

			Expect(func() {
				emptyScatter.added(partitionDevice("nvme0n1p1", "nvme_disk01"))
			}).NotTo(Panic())
			Expect(emptyScatter.routes).To(BeEmpty())
		})
	})

	Describe("removed with an existing route", func() {
		It("submits a HealthEvent when a matching device is removed", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_disk01")
			scatter.removed(dev)
			Eventually(watchCh).Should(Receive())
		})

		It("does nothing for a non-matching device", func() {
			dev := partitionDevice("sda1", "data_01")
			scatter.removed(dev)
			Consistently(watchCh, 50*time.Millisecond).ShouldNot(Receive())
		})
	})

	// Regression: udevDiscovery has a TOCTOU between NewEnumerate and
	// NewMonitorFromNetlink. A device added in that gap is never recorded
	// in state, so its later removal produces Removed{nil}.
	Describe("nil device from TOCTOU gap", func() {
		It("does not panic when added receives a nil device", func() {
			Expect(func() { scatter.added(nil) }).NotTo(Panic())
		})

		It("does not panic when removed receives a nil device", func() {
			Expect(func() { scatter.removed(nil) }).NotTo(Panic())
		})
	})
})
