package plugin

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/ydb-platform/udev-manager/internal/udev"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NetBWMatcherTemplater", func() {
	var matcher *regexp.Regexp

	BeforeEach(func() {
		matcher = regexp.MustCompile(`eth(.*)`)
	})

	It("returns nil for a non-net device", func() {
		dev := partitionDevice("sda1", "data_01")
		tmpl, err := NetBWMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns nil when the INTERFACE property is missing", func() {
		dev := &mockDevice{subsystem: "net", properties: map[string]string{}}
		tmpl, err := NetBWMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns nil when the interface name does not match", func() {
		dev := netDevice("wlan0", "100", "up")
		tmpl, err := NetBWMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).To(BeNil())
	})

	It("returns a template using the capture group as suffix", func() {
		dev := netDevice("eth0", "1000", "up")
		tmpl, err := NetBWMatcherTemplater("ydb.tech", matcher)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(tmpl).NotTo(BeNil())
		Expect(tmpl.Domain).To(Equal("ydb.tech"))
		Expect(tmpl.Prefix).To(Equal("netbw-0"))
	})
})

var _ = Describe("NetBWMatcherInstances", func() {
	It("returns nil for a non-net device", func() {
		matcher := regexp.MustCompile(`eth.*`)
		dev := partitionDevice("sda1", "data_01")
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 100)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(instances).To(BeNil())
	})

	It("returns nil when the INTERFACE property is missing", func() {
		matcher := regexp.MustCompile(`eth.*`)
		dev := &mockDevice{subsystem: "net", properties: map[string]string{}, sysattrs: map[string]string{}}
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 100)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(instances).To(BeNil())
	})

	It("returns an error when the speed attribute cannot be read", func() {
		matcher := regexp.MustCompile(`eth.*`)
		dev := netDevice("eth0", "", "up")
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 100)(dev)
		Expect(err).To(MatchError(ContainSubstring(`network interface "eth0" has no "speed" attribute`)))
		Expect(instances).To(BeNil())
	})

	It("returns an error when the speed attribute is malformed", func() {
		matcher := regexp.MustCompile(`eth.*`)
		dev := netDevice("eth0", "not-a-number", "up")
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 100)(dev)
		Expect(err).To(MatchError(ContainSubstring(`parse "eth0" speed "not-a-number"`)))
		Expect(instances).To(BeNil())
	})

	It("returns an error when the speed is reported as unavailable", func() {
		matcher := regexp.MustCompile(`eth.*`)
		dev := netDevice("eth0", "-1", "up")
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 100)(dev)
		Expect(err).To(MatchError(ContainSubstring(`network interface "eth0" has invalid speed -1 Mbps`)))
		Expect(instances).To(BeNil())
	})

	It("returns an error instead of panicking when bandwidth per share is zero", func() {
		matcher := regexp.MustCompile(`eth.*`)
		dev := netDevice("eth0", "1000", "up")
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 0)(dev)
		Expect(err).To(MatchError("bandwidth per share must be positive"))
		Expect(instances).To(BeNil())
	})

	It("returns nil when shares would be zero (mbpsPerShare exceeds speed)", func() {
		matcher := regexp.MustCompile(`eth.*`)
		dev := netDevice("eth0", "100", "up")
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 1000)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(instances).To(BeNil())
	})

	It("returns the correct number of shares", func() {
		matcher := regexp.MustCompile(`eth.*`)
		dev := netDevice("eth0", "1000", "up")
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 100)(dev)
		Expect(err).NotTo(HaveOccurred())
		Expect(instances).To(HaveLen(10)) // 1000 Mbps / 100 MbpsPerShare = 10
	})

	It("assigns sequential IDs in ifname_N format", func() {
		matcher := regexp.MustCompile(`eth.*`)
		dev := netDevice("eth0", "1000", "up")
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 100)(dev)
		Expect(err).NotTo(HaveOccurred())
		for i, inst := range instances {
			Expect(string(inst.Id())).To(Equal(fmt.Sprintf("eth0_%d", i)))
		}
	})

	It("leaves the last good resource state intact after a transient speed read failure", func() {
		matcher := regexp.MustCompile(`eth(.*)`)
		template := ResourceTemplate{Domain: "ydb.tech", Prefix: "netbw-0"}
		goodDev := netDevice("eth0", "100", "up")
		instances, err := NetBWMatcherInstances("ydb.tech", matcher, 100)(goodDev)
		Expect(err).NotTo(HaveOccurred())
		Expect(instances).To(HaveLen(1))

		res := newResource(template, map[Id]Instance{instances[0].Id(): instances[0]})
		DeferCleanup(res.Close)
		ctx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)
		updates := res.Watch(ctx)
		Eventually(updates).Should(Receive()) // initial state

		scatter := &Scatter[*networkBandwidth]{
			templater: NetBWMatcherTemplater("ydb.tech", matcher),
			mapper:    NetBWMatcherInstances("ydb.tech", matcher, 100),
			routes:    map[ResourceTemplate]Resource{template: res},
			known:     make(map[ResourceTemplate]map[Id]Instance),
		}
		badDev := netDevice("eth0", "", "up")
		scatter.applySnapshot(udev.Snapshot{Generation: 2, Devices: []udev.Device{badDev}})

		Expect(res.Devices()).To(ConsistOf(And(
			HaveField("ID", "eth0_0"),
			HaveField("Health", "Healthy"),
		)))
		Expect(baseInstance(res.Instances()["eth0_0"]).(*networkBandwidth).dev).To(BeIdenticalTo(goodDev))
		Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
	})
})

var _ = Describe("networkBandwidth", func() {
	Describe("Id", func() {
		It("returns ifname_idx format", func() {
			n := &networkBandwidth{ifname: "eth0", idx: 3}
			Expect(string(n.Id())).To(Equal("eth0_3"))
		})
	})

	Describe("Health", func() {
		It("is Healthy when operstate is up", func() {
			dev := netDevice("eth0", "1000", "up")
			n := &networkBandwidth{ifname: "eth0", idx: 0, dev: dev}
			Expect(n.Health()).To(BeAssignableToTypeOf(Healthy{}))
		})

		It("is Unhealthy when operstate is down", func() {
			dev := netDevice("eth0", "1000", "down")
			n := &networkBandwidth{ifname: "eth0", idx: 0, dev: dev}
			Expect(n.Health()).To(BeAssignableToTypeOf(Unhealthy{}))
		})

		It("is Unhealthy when operstate is unknown", func() {
			dev := netDevice("eth0", "1000", "unknown")
			n := &networkBandwidth{ifname: "eth0", idx: 0, dev: dev}
			Expect(n.Health()).To(BeAssignableToTypeOf(Unhealthy{}))
		})
	})

	Describe("TopologyHints", func() {
		It("always returns nil", func() {
			dev := netDevice("eth0", "1000", "up")
			n := &networkBandwidth{ifname: "eth0", idx: 0, dev: dev}
			Expect(n.TopologyHints()).To(BeNil())
		})
	})

	Describe("Allocate", func() {
		It("returns an empty response (soft resource, no devices passed through)", func() {
			dev := netDevice("eth0", "1000", "up")
			n := &networkBandwidth{ifname: "eth0", idx: 0, dev: dev}
			resp, err := n.Allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Devices).To(BeEmpty())
			Expect(resp.Mounts).To(BeEmpty())
			Expect(resp.Envs).To(BeEmpty())
		})
	})
})
