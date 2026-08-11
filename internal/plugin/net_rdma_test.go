package plugin

import (
	"context"
	"errors"
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

var _ = Describe("NetRdmaMatcherInstances", func() {
	It("returns an error when the RDMA device lookup fails", func() {
		const ifname = "rdma-interface-that-cannot-exist"
		matcher := regexp.MustCompile(`.*`)
		dev := netDevice(ifname, "100000", "up")

		instances, err := NetRdmaMatcherInstances("ydb.tech", matcher, 1)(dev)

		Expect(err).To(MatchError(ContainSubstring(`get RDMA device for network interface "` + ifname + `"`)))
		Expect(instances).To(BeNil())
	})

	It("does not let a broad matcher's non-RDMA interface block a valid template", func() {
		matcher := regexp.MustCompile(`.*`)
		validTemplate := ResourceTemplate{Domain: "ydb.tech", Prefix: "netrdma-"}
		validResource := newResource(validTemplate, map[Id]Instance{})
		DeferCleanup(validResource.Close)
		updates := validResource.Watch(context.Background())
		Eventually(updates).Should(Receive())

		lookupDevice := func(ifname string) (string, error) {
			if ifname == "eth0" {
				return "", errors.New("rdma device not found for netdev eth0")
			}
			return "mlx5_0", nil
		}
		lookupCharDevices := func(string) []string { return []string{"/dev/infiniband/uverbs0"} }
		scatter := &Scatter[*netRdma]{
			templater: NetRdmaMatcherTemplater("ydb.tech", matcher),
			mapper: netRdmaMatcherInstances(
				"ydb.tech",
				matcher,
				1,
				lookupDevice,
				lookupCharDevices,
			),
			routes: map[ResourceTemplate]Resource{validTemplate: validResource},
			known:  make(map[ResourceTemplate]map[Id]Instance),
		}

		nonRDMA := netDevice("eth0", "1000", "up")
		valid := netDevice("rdma0", "100000", "up")
		scatter.applySnapshot(udev.Snapshot{
			Generation: 1,
			Devices:    []udev.Device{nonRDMA, valid},
		})

		Eventually(updates).Should(Receive())
		Expect(validResource.Devices()).To(ConsistOf(And(
			HaveField("ID", "rdma0_0"),
			HaveField("Health", "Healthy"),
		)))
	})

	DescribeTable("treats definitive non-RDMA lookup results as a non-match",
		func(rdmaDevice string, lookupErr error) {
			charDevicesCalled := false
			mapper := netRdmaMatcherInstances(
				"ydb.tech",
				regexp.MustCompile(`.*`),
				1,
				func(string) (string, error) { return rdmaDevice, lookupErr },
				func(string) []string {
					charDevicesCalled = true
					return nil
				},
			)

			instances, err := mapper(netDevice("eth0", "1000", "up"))

			Expect(err).NotTo(HaveOccurred())
			Expect(instances).To(BeNil())
			Expect(charDevicesCalled).To(BeFalse())
		},
		Entry("when Ethernet has no RDMA mapping", "", errors.New("rdma device not found for netdev eth0")),
		Entry("when IPoIB lookup returns an empty device", "", nil),
		Entry("when the link type cannot support RDMA", "", errors.New("unknown device type")),
	)

	It("preserves transient RDMA lookup errors", func() {
		mapper := netRdmaMatcherInstances(
			"ydb.tech",
			regexp.MustCompile(`.*`),
			1,
			func(string) (string, error) { return "", errors.New("temporary sysfs read failure") },
			func(string) []string { return nil },
		)

		instances, err := mapper(netDevice("eth0", "1000", "up"))

		Expect(err).To(MatchError(ContainSubstring("temporary sysfs read failure")))
		Expect(instances).To(BeNil())
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
