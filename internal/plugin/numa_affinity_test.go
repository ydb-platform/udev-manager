package plugin

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("numaAffinityDevice", func() {
	var dev *numaAffinityDevice

	BeforeEach(func() {
		dev = &numaAffinityDevice{id: "0", numaNode: 3}
	})

	It("returns the configured Id", func() {
		Expect(dev.Id()).To(Equal(Id("0")))
	})

	It("is always Healthy", func() {
		Expect(dev.Health()).To(BeAssignableToTypeOf(Healthy{}))
	})

	It("reports topology hints for the configured NUMA node", func() {
		hints := dev.TopologyHints()
		Expect(hints).NotTo(BeNil())
		Expect(hints.Nodes).To(HaveLen(1))
		Expect(hints.Nodes[0].ID).To(BeEquivalentTo(3))
	})

	It("allocates an empty response (no devices, mounts, or envs)", func() {
		resp, err := dev.Allocate(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Devices).To(BeEmpty())
		Expect(resp.Mounts).To(BeEmpty())
		Expect(resp.Envs).To(BeEmpty())
	})
})
