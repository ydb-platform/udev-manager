package plugin

import (
	"context"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ydb-platform/udev-manager/internal/udev"
)

var _ = Describe("NetRdmaMatcherTemplater", func() {
	var matcher *regexp.Regexp

	BeforeEach(func() {
		matcher = regexp.MustCompile(`ib(.*)`)
	})

	It("returns nil for a non-net device", func() {
		dev := partitionDevice("sda1", "data_01")
		tmpl, err := NetRdmaMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns nil when the INTERFACE property is missing", func() {
		dev := &mockDevice{subsystem: udev.NetSubsystem, properties: map[string]string{}}
		tmpl, err := NetRdmaMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns nil when the interface name does not match", func() {
		dev := netDevice("eth0", "1000", "up")
		tmpl, err := NetRdmaMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns a template using the capture group as suffix", func() {
		dev := netDevice("ib0", "100000", "up")
		tmpl, err := NetRdmaMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).NotTo(BeNil())
		Expect(tmpl.Domain).To(Equal("ydb.tech"))
		Expect(tmpl.Prefix).To(Equal("netrdma-0"))
	})
})

var _ = Describe("netRdma", func() {
	Describe("Id", func() {
		It("returns ifname_idx format", func() {
			n := &netRdma{ifname: "ib0", idx: 2}
			Expect(string(n.Id())).To(Equal("ib0_2"))
		})
	})

	Describe("Health", func() {
		It("is Healthy when operstate is up", func() {
			dev := netDevice("ib0", "100000", "up")
			n := &netRdma{domain: "ydb.tech", ifname: "ib0", idx: 0, dev: dev}
			Expect(n.Health()).To(BeAssignableToTypeOf(Healthy{}))
		})

		It("is Unhealthy when operstate is down", func() {
			dev := netDevice("ib0", "100000", "down")
			n := &netRdma{domain: "ydb.tech", ifname: "ib0", idx: 0, dev: dev}
			Expect(n.Health()).To(BeAssignableToTypeOf(Unhealthy{}))
		})
	})

	Describe("TopologyHints", func() {
		It("always returns nil", func() {
			dev := netDevice("ib0", "100000", "up")
			n := &netRdma{ifname: "ib0", idx: 0, dev: dev}
			Expect(n.TopologyHints()).To(BeNil())
		})
	})

	Describe("Allocate", func() {
		It("maps each associated device with identical host and container paths", func() {
			dev := netDevice("ib0", "100000", "up")
			n := &netRdma{
				domain: "ydb.tech",
				ifname: "ib0",
				idx:    0,
				dev:    dev,
				associatedDevices: []string{
					"/dev/infiniband/uverbs0",
					"/dev/infiniband/rdma_cm",
				},
			}
			resp, err := n.Allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Devices).To(HaveLen(2))
			Expect(resp.Devices[0].HostPath).To(Equal("/dev/infiniband/uverbs0"))
			Expect(resp.Devices[0].ContainerPath).To(Equal("/dev/infiniband/uverbs0"))
			Expect(resp.Devices[0].Permissions).To(Equal("rw"))
			Expect(resp.Devices[1].HostPath).To(Equal("/dev/infiniband/rdma_cm"))
		})

		It("returns an empty response when there are no associated devices", func() {
			dev := netDevice("ib0", "100000", "up")
			n := &netRdma{domain: "ydb.tech", ifname: "ib0", idx: 0, dev: dev, associatedDevices: nil}
			resp, err := n.Allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Devices).To(BeEmpty())
		})
	})
})
