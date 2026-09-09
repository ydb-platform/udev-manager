package plugin

import (
	"context"
	"errors"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ydb-platform/udev-manager/internal/udev"
)

func netDeviceWithPCIParent(ifname string, sysattrs map[string]string) *mockDevice {
	dev := netDevice(ifname, "100000", "up")
	dev.parent = &mockDevice{
		id:        udev.Id("/sys/devices/pci0000:00/0000:01:00.0"),
		subsystem: "pci",
		sysattrs:  sysattrs,
	}
	return dev
}

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

var _ = Describe("classifyNetRdmaDevice", func() {
	DescribeTable("classifies the closest PCI function",
		func(sysattrs map[string]string, expected NetRdmaDeviceType) {
			deviceType, err := classifyNetRdmaDevice(netDeviceWithPCIParent("eth0", sysattrs))
			Expect(err).NotTo(HaveOccurred())
			Expect(deviceType).To(Equal(expected))
		},
		Entry("VF by physfn", map[string]string{"physfn": "../0000:01:00.0"}, NetRdmaDeviceTypeVF),
		Entry("PF by positive sriov_totalvfs", map[string]string{"sriov_totalvfs": "8"}, NetRdmaDeviceTypePF),
		Entry("VF takes precedence", map[string]string{"physfn": "../0000:01:00.0", "sriov_totalvfs": "8"}, NetRdmaDeviceTypeVF),
		Entry("zero total VFs is neither", map[string]string{"sriov_totalvfs": "0"}, netRdmaDeviceTypeUnknown),
		Entry("missing SR-IOV attributes is neither", map[string]string{}, netRdmaDeviceTypeUnknown),
	)

	It("walks through an intermediate parent to the closest PCI function", func() {
		dev := netDevice("ib0", "100000", "up")
		dev.parent = &mockDevice{
			subsystem: "infiniband",
			parent: &mockDevice{
				subsystem: "pci",
				sysattrs:  map[string]string{"sriov_totalvfs": "4"},
			},
		}

		deviceType, err := classifyNetRdmaDevice(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(deviceType).To(Equal(NetRdmaDeviceTypePF))
	})

	It("returns neither when there is no PCI ancestor", func() {
		dev := netDevice("vlan0", "100000", "up")
		dev.parent = &mockDevice{subsystem: "virtual", sysattrs: map[string]string{}}

		deviceType, err := classifyNetRdmaDevice(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(deviceType).To(Equal(netRdmaDeviceTypeUnknown))
	})

	It("rejects a malformed sriov_totalvfs value", func() {
		_, err := classifyNetRdmaDevice(netDeviceWithPCIParent("eth0", map[string]string{
			"sriov_totalvfs": "invalid",
		}))
		Expect(err).To(MatchError(ContainSubstring("sriov_totalvfs")))
	})
})

var _ = Describe("NetRdmaMatcherInstances", func() {
	newMapper := func(
		deviceType NetRdmaDeviceType,
		lookupDevice func(string) (string, error),
		lookupCharDevices func(string) []string,
		classifyDevice func(udev.Device) (NetRdmaDeviceType, error),
	) FromDevice[[]*netRdma] {
		return netRdmaMatcherInstances(
			"ydb.tech",
			regexp.MustCompile(`.*`),
			2,
			deviceType,
			lookupDevice,
			lookupCharDevices,
			classifyDevice,
		)
	}

	DescribeTable("treats definitive non-RDMA lookup results as a non-match",
		func(rdmaDevice string, lookupErr error) {
			charDevicesCalled := false
			mapper := newMapper(
				NetRdmaDeviceTypeAny,
				func(string) (string, error) { return rdmaDevice, lookupErr },
				func(string) []string {
					charDevicesCalled = true
					return nil
				},
				func(udev.Device) (NetRdmaDeviceType, error) {
					Fail("device classification must be skipped when deviceType is omitted")
					return netRdmaDeviceTypeUnknown, nil
				},
			)

			instances, err := mapper(netDevice("eth0", "1000", "up"))

			Expect(err).NotTo(HaveOccurred())
			Expect(instances).To(BeNil())
			Expect(charDevicesCalled).To(BeFalse())
		},
		Entry("Ethernet has no RDMA mapping", "", errors.New("rdma device not found for netdev eth0")),
		Entry("IPoIB lookup returns an empty mapping", "", nil),
		Entry("link type cannot support RDMA", "", errors.New("unknown device type")),
	)

	It("returns unexpected RDMA lookup failures", func() {
		mapper := newMapper(
			NetRdmaDeviceTypeAny,
			func(string) (string, error) { return "", errors.New("temporary sysfs failure") },
			func(string) []string { return nil },
			func(udev.Device) (NetRdmaDeviceType, error) { return netRdmaDeviceTypeUnknown, nil },
		)

		instances, err := mapper(netDevice("eth0", "1000", "up"))

		Expect(err).To(MatchError(ContainSubstring("temporary sysfs failure")))
		Expect(instances).To(BeNil())
	})

	It("uses a non-empty RDMA mapping even when no character devices are found", func() {
		mapper := newMapper(
			NetRdmaDeviceTypeAny,
			func(string) (string, error) { return "mlx5_0", nil },
			func(string) []string { return nil },
			func(udev.Device) (NetRdmaDeviceType, error) {
				Fail("device classification must be skipped when deviceType is omitted")
				return netRdmaDeviceTypeUnknown, nil
			},
		)

		instances, err := mapper(netDevice("eth0", "100000", "up"))

		Expect(err).NotTo(HaveOccurred())
		Expect(instances).To(HaveLen(2))
		Expect(instances[0].associatedDevices).To(BeEmpty())
	})

	DescribeTable("filters interfaces by the requested device type",
		func(requested, detected NetRdmaDeviceType, expectedInstances int) {
			lookupCalled := false
			mapper := newMapper(
				requested,
				func(string) (string, error) {
					lookupCalled = true
					return "mlx5_0", nil
				},
				func(string) []string { return []string{"/dev/infiniband/uverbs0"} },
				func(udev.Device) (NetRdmaDeviceType, error) { return detected, nil },
			)

			instances, err := mapper(netDevice("eth0", "100000", "up"))

			Expect(err).NotTo(HaveOccurred())
			Expect(instances).To(HaveLen(expectedInstances))
			Expect(lookupCalled).To(Equal(expectedInstances > 0))
		},
		Entry("PF accepts PF", NetRdmaDeviceTypePF, NetRdmaDeviceTypePF, 2),
		Entry("PF rejects VF", NetRdmaDeviceTypePF, NetRdmaDeviceTypeVF, 0),
		Entry("PF rejects unknown", NetRdmaDeviceTypePF, netRdmaDeviceTypeUnknown, 0),
		Entry("VF accepts VF", NetRdmaDeviceTypeVF, NetRdmaDeviceTypeVF, 2),
		Entry("VF rejects PF", NetRdmaDeviceTypeVF, NetRdmaDeviceTypePF, 0),
		Entry("VF rejects unknown", NetRdmaDeviceTypeVF, netRdmaDeviceTypeUnknown, 0),
	)

	It("returns device classification failures", func() {
		lookupCalled := false
		mapper := newMapper(
			NetRdmaDeviceTypePF,
			func(string) (string, error) {
				lookupCalled = true
				return "mlx5_0", nil
			},
			func(string) []string { return nil },
			func(udev.Device) (NetRdmaDeviceType, error) {
				return netRdmaDeviceTypeUnknown, errors.New("classification failed")
			},
		)

		instances, err := mapper(netDevice("eth0", "100000", "up"))

		Expect(err).To(MatchError(ContainSubstring("classification failed")))
		Expect(instances).To(BeNil())
		Expect(lookupCalled).To(BeFalse())
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
