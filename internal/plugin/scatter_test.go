package plugin

import (
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// newTestResource creates a resource with a buffered healthCh suitable for unit tests.
func newTestResource(domain, prefix string) *resource {
	return &resource{
		resourceTemplate: ResourceTemplate{Domain: domain, Prefix: prefix},
		instances:        make(map[Id]Instance),
		healthCh:         make(chan HealthEvent, 10),
	}
}

var _ = Describe("Scatter", func() {
	var (
		matcher *regexp.Regexp
		tmpl    ResourceTemplate
		res     *resource
		scatter *Scatter[*partition]
	)

	BeforeEach(func() {
		matcher = regexp.MustCompile(`nvme_(.*)`)
		tmpl = ResourceTemplate{Domain: "ydb.tech", Prefix: "part-disk01"}
		res = newTestResource("ydb.tech", "part-disk01")

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
			Eventually(res.healthCh).Should(Receive())
		})

		It("the submitted event carries the matched instance", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_disk01")
			scatter.added(dev)
			Eventually(res.healthCh).Should(Receive(
				WithTransform(func(ev HealthEvent) int { return len(ev.Instances) }, Equal(1)),
			))
		})

		It("does nothing for a non-matching device", func() {
			dev := partitionDevice("sda1", "data_01")
			scatter.added(dev)
			Consistently(res.healthCh, 50*time.Millisecond).ShouldNot(Receive())
		})
	})

	Describe("removed with an existing route", func() {
		It("submits a HealthEvent when a matching device is removed", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_disk01")
			scatter.removed(dev)
			Eventually(res.healthCh).Should(Receive())
		})

		It("does nothing for a non-matching device", func() {
			dev := partitionDevice("sda1", "data_01")
			scatter.removed(dev)
			Consistently(res.healthCh, 50*time.Millisecond).ShouldNot(Receive())
		})
	})
})
