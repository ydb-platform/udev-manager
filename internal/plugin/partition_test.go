package plugin

import (
	"context"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ydb-platform/udev-manager/internal/udev"
)

var _ = Describe("sanitizeEnv", func() {
	DescribeTable("replaces special chars and uppercases",
		func(input, expected string) {
			Expect(sanitizeEnv(input)).To(Equal(expected))
		},
		Entry("dots replaced with underscores", "my.domain", "MY_DOMAIN"),
		Entry("hyphens replaced with underscores", "my-label", "MY_LABEL"),
		Entry("mixed dots and hyphens", "foo.bar-baz", "FOO_BAR_BAZ"),
		Entry("already uppercase plain string", "PLAIN", "PLAIN"),
		Entry("empty string", "", ""),
		Entry("single char", "a", "A"),
	)
})

var _ = Describe("allocatePartitionDevice", func() {
	var dev *mockDevice

	BeforeEach(func() {
		dev = partitionDevice("sda1", "data_01")
		dev.sysattrs = map[string]string{
			udev.SysAttrWWID:   "eui.test123",
			udev.SysAttrModel:  "SAMSUNG SSD",
			udev.SysAttrSerial: "S001",
		}
	})

	It("creates a single device spec", func() {
		resp := allocatePartitionDevice(dev, "ydb.tech", "data_01")
		Expect(resp.Devices).To(HaveLen(1))
	})

	It("sets correct host and container paths", func() {
		resp := allocatePartitionDevice(dev, "ydb.tech", "data_01")
		Expect(resp.Devices[0].HostPath).To(Equal("/dev/sda1"))
		Expect(resp.Devices[0].ContainerPath).To(Equal("/dev/allocated/ydb.tech/part/data_01"))
		Expect(resp.Devices[0].Permissions).To(Equal("rw"))
	})

	It("sets PATH env var to container path", func() {
		resp := allocatePartitionDevice(dev, "ydb.tech", "data_01")
		Expect(resp.Envs["YDB_TECH_PART_DATA_01_PATH"]).To(Equal("/dev/allocated/ydb.tech/part/data_01"))
	})

	It("sets DISK_ID env var from wwid sysattr", func() {
		resp := allocatePartitionDevice(dev, "ydb.tech", "data_01")
		Expect(resp.Envs["YDB_TECH_PART_DATA_01_DISK_ID"]).To(Equal("eui.test123"))
	})

	It("sets DISK_MODEL env var from model sysattr", func() {
		resp := allocatePartitionDevice(dev, "ydb.tech", "data_01")
		Expect(resp.Envs["YDB_TECH_PART_DATA_01_DISK_MODEL"]).To(Equal("SAMSUNG SSD"))
	})

	It("sets DISK_SERIAL env var from serial sysattr", func() {
		resp := allocatePartitionDevice(dev, "ydb.tech", "data_01")
		Expect(resp.Envs["YDB_TECH_PART_DATA_01_DISK_SERIAL"]).To(Equal("S001"))
	})

	It("falls back to PropertyShortSerial when serial sysattr is empty", func() {
		dev.sysattrs[udev.SysAttrSerial] = ""
		dev.properties[udev.PropertyShortSerial] = "PROP_SERIAL"
		resp := allocatePartitionDevice(dev, "ydb.tech", "data_01")
		Expect(resp.Envs["YDB_TECH_PART_DATA_01_DISK_SERIAL"]).To(Equal("PROP_SERIAL"))
	})

	It("omits DISK_SERIAL when neither sysattr nor property has a serial", func() {
		dev.sysattrs[udev.SysAttrSerial] = ""
		resp := allocatePartitionDevice(dev, "ydb.tech", "data_01")
		Expect(resp.Envs).NotTo(HaveKey("YDB_TECH_PART_DATA_01_DISK_SERIAL"))
	})

	It("sanitizes domain and label in env var names", func() {
		resp := allocatePartitionDevice(dev, "my.domain", "my-label")
		Expect(resp.Envs).To(HaveKey("MY_DOMAIN_PART_MY_LABEL_PATH"))
	})
})

var _ = Describe("PartitionLabelMatcherTemplater", func() {
	var matcher *regexp.Regexp

	BeforeEach(func() {
		matcher = regexp.MustCompile("nvme_(.*)")
	})

	It("returns nil for a non-block device", func() {
		dev := netDevice("eth0", "1000", "up")
		tmpl, err := PartitionLabelMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns nil for a block disk (not a partition)", func() {
		dev := &mockDevice{
			id:         udev.Id("sda"),
			subsystem:  udev.BlockSubsystem,
			devType:    "disk",
			properties: map[string]string{},
		}
		tmpl, err := PartitionLabelMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns nil for a partition without a PARTNAME property", func() {
		dev := &mockDevice{
			id:         udev.Id("sda1"),
			subsystem:  udev.BlockSubsystem,
			devType:    udev.DeviceTypePart,
			properties: map[string]string{},
		}
		tmpl, err := PartitionLabelMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns nil when the label does not match the regexp", func() {
		dev := partitionDevice("sda1", "data_01")
		tmpl, err := PartitionLabelMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns a template with capture group as the suffix", func() {
		dev := partitionDevice("nvme0n1p1", "nvme_disk01")
		tmpl, err := PartitionLabelMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).NotTo(BeNil())
		Expect(tmpl.Domain).To(Equal("ydb.tech"))
		Expect(tmpl.Prefix).To(Equal("part-disk01"))
	})

	It("uses an empty suffix when the regexp has no capture groups", func() {
		matcher = regexp.MustCompile("nvme_disk01")
		dev := partitionDevice("nvme0n1p1", "nvme_disk01")
		tmpl, err := PartitionLabelMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).NotTo(BeNil())
		Expect(tmpl.Prefix).To(Equal("part-"))
	})
})

var _ = Describe("PartitionLabelMatcherInstances", func() {
	var matcher *regexp.Regexp

	BeforeEach(func() {
		matcher = regexp.MustCompile("nvme_(.*)")
	})

	It("returns nil for a non-matching device", func() {
		dev := partitionDevice("sda1", "data_01")
		instances, err := PartitionLabelMatcherInstances("ydb.tech", matcher, false)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(instances).To(BeNil())
	})

	It("returns one partition instance with the capture group as its ID", func() {
		dev := partitionDevice("nvme0n1p1", "nvme_disk01")
		instances, err := PartitionLabelMatcherInstances("ydb.tech", matcher, false)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(instances).To(HaveLen(1))
		Expect(string(instances[0].Id())).To(Equal("disk01"))
	})

	It("uses the full label as ID when the regexp has no capture group", func() {
		matcher = regexp.MustCompile("nvme_disk01")
		dev := partitionDevice("nvme0n1p1", "nvme_disk01")
		instances, err := PartitionLabelMatcherInstances("ydb.tech", matcher, false)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(instances).To(HaveLen(1))
		Expect(string(instances[0].Id())).To(Equal("nvme_disk01"))
	})
})

var _ = Describe("partition", func() {
	var dev *mockDevice

	BeforeEach(func() {
		dev = partitionDevice("nvme0n1p1", "disk01")
		dev.numaNode = 1
	})

	Describe("Health", func() {
		It("is always Healthy", func() {
			p := &partition{label: "disk01", domain: "ydb.tech", dev: dev}
			Expect(p.Health()).To(BeAssignableToTypeOf(Healthy{}))
		})
	})

	Describe("TopologyHints", func() {
		It("returns NUMA node info when available", func() {
			p := &partition{label: "disk01", domain: "ydb.tech", dev: dev, disableTopologyHints: false}
			hints := p.TopologyHints()
			Expect(hints).NotTo(BeNil())
			Expect(hints.Nodes).To(HaveLen(1))
			Expect(hints.Nodes[0].ID).To(BeEquivalentTo(1))
		})

		It("returns nil when topology hints are disabled", func() {
			p := &partition{label: "disk01", domain: "ydb.tech", dev: dev, disableTopologyHints: true}
			Expect(p.TopologyHints()).To(BeNil())
		})

		It("returns nil when NumaNode is negative", func() {
			dev.numaNode = -1
			p := &partition{label: "disk01", domain: "ydb.tech", dev: dev, disableTopologyHints: false}
			Expect(p.TopologyHints()).To(BeNil())
		})
	})

	Describe("Allocate", func() {
		It("returns a response with the correct device spec and env vars", func() {
			p := &partition{label: "disk01", domain: "ydb.tech", dev: dev}
			resp, err := p.Allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Devices).To(HaveLen(1))
			Expect(resp.Envs).To(HaveKey("YDB_TECH_PART_DISK01_PATH"))
		})
	})
})
