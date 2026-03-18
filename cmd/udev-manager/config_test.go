package main

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// parseYAML is a test helper that parses a YAML string into a appConfig.
func parseYAML(yaml string) (*appConfig, error) {
	return parseConfig(strings.NewReader(yaml))
}

// mustParseYAML parses YAML and fails the test on error.
func mustParseYAML(yaml string) *appConfig {
	cfg, err := parseYAML(yaml)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return cfg
}

const minimalValidConfig = `
domain: ydb.tech
`

var _ = Describe("parseConfig", func() {
	It("accepts a minimal valid config", func() {
		cfg, err := parseYAML(minimalValidConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.DeviceDomain).To(Equal("ydb.tech"))
	})

	It("rejects an empty config", func() {
		_, err := parseYAML(`{}`)
		Expect(err).To(HaveOccurred())
	})

	It("rejects config with missing domain", func() {
		_, err := parseYAML(`partitions: []`)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(".domain"))
	})

	It("rejects config with an invalid domain", func() {
		_, err := parseYAML(`domain: "not a domain"`)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(".domain"))
	})

	It("defaults health_check_port to 8080 when not set", func() {
		cfg := mustParseYAML(minimalValidConfig)
		Expect(cfg.HealthCheckPort).To(BeEquivalentTo(defaultHealthcheckPort))
	})

	It("respects an explicit health_check_port", func() {
		cfg := mustParseYAML(`
domain: ydb.tech
health_check_port: 9090
`)
		Expect(cfg.HealthCheckPort).To(BeEquivalentTo(9090))
	})

	It("accepts disable_topology_hints flag", func() {
		cfg := mustParseYAML(`
domain: ydb.tech
disable_topology_hints: true
`)
		Expect(cfg.DisableTopologyHints).To(BeTrue())
	})

	It("collects all validation errors rather than stopping at the first", func() {
		// Two invalid partition entries — both errors should appear.
		_, err := parseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)(extra)"
  - matcher: "["
`)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(".partitions[0]"))
		Expect(err.Error()).To(ContainSubstring(".partitions[1]"))
	})
})

var _ = Describe("partitionsConfig.validate", func() {
	It("accepts a simple matcher", func() {
		pc := &partitionsConfig{Matcher: `nvme_(.*)`}
		Expect(pc.validate()).NotTo(HaveOccurred())
		Expect(pc.matcher).NotTo(BeNil())
	})

	It("accepts a matcher with no capture group", func() {
		pc := &partitionsConfig{Matcher: `nvme.*`}
		Expect(pc.validate()).NotTo(HaveOccurred())
	})

	It("rejects an invalid regexp", func() {
		pc := &partitionsConfig{Matcher: `[`}
		Expect(pc.validate()).To(MatchError(ContainSubstring(".matcher")))
	})

	It("rejects a matcher with more than one capture group", func() {
		pc := &partitionsConfig{Matcher: `nvme_(.*)_(.*)`}
		err := pc.validate()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("at most one capturing group"))
	})

	It("accepts exactly one capture group", func() {
		pc := &partitionsConfig{Matcher: `nvme_(.*)`}
		Expect(pc.validate()).NotTo(HaveOccurred())
	})

	It("accepts an empty domain override (uses global domain)", func() {
		pc := &partitionsConfig{Matcher: `nvme_(.*)`, DomainOverride: ""}
		Expect(pc.validate()).NotTo(HaveOccurred())
	})

	It("accepts a valid domain override", func() {
		pc := &partitionsConfig{Matcher: `nvme_(.*)`, DomainOverride: "storage.example.com"}
		Expect(pc.validate()).NotTo(HaveOccurred())
	})

	It("rejects an invalid domain override", func() {
		pc := &partitionsConfig{Matcher: `nvme_(.*)`, DomainOverride: "not a domain"}
		Expect(pc.validate()).To(MatchError(ContainSubstring(".domain")))
	})

	It("compiles matcher so it can be used after validate", func() {
		pc := &partitionsConfig{Matcher: `nvme_(disk\d+)`}
		Expect(pc.validate()).NotTo(HaveOccurred())
		Expect(pc.matcher.MatchString("nvme_disk01")).To(BeTrue())
		Expect(pc.matcher.MatchString("sda1")).To(BeFalse())
	})
})

var _ = Describe("batchPartitionsConfig.validate", func() {
	It("accepts a minimal valid batch config", func() {
		bc := &batchPartitionsConfig{Name: "nvme-set", Matcher: `nvme.*`}
		Expect(bc.validate()).NotTo(HaveOccurred())
		Expect(bc.matcher).NotTo(BeNil())
	})

	It("rejects an empty name", func() {
		bc := &batchPartitionsConfig{Name: "", Matcher: `nvme.*`}
		Expect(bc.validate()).To(MatchError(ContainSubstring(".name")))
	})

	It("rejects an invalid regexp", func() {
		bc := &batchPartitionsConfig{Name: "test", Matcher: `[`}
		Expect(bc.validate()).To(MatchError(ContainSubstring(".matcher")))
	})

	It("defaults count to 1 when not set", func() {
		bc := &batchPartitionsConfig{Name: "nvme-set", Matcher: `nvme.*`}
		Expect(bc.validate()).NotTo(HaveOccurred())
		Expect(bc.Count).To(Equal(1))
	})

	It("preserves an explicit count", func() {
		bc := &batchPartitionsConfig{Name: "nvme-set", Matcher: `nvme.*`, Count: 3}
		Expect(bc.validate()).NotTo(HaveOccurred())
		Expect(bc.Count).To(Equal(3))
	})

	It("accepts a valid domain override", func() {
		bc := &batchPartitionsConfig{Name: "nvme-set", Matcher: `nvme.*`, DomainOverride: "storage.example.com"}
		Expect(bc.validate()).NotTo(HaveOccurred())
	})

	It("rejects an invalid domain override", func() {
		bc := &batchPartitionsConfig{Name: "nvme-set", Matcher: `nvme.*`, DomainOverride: "bad domain"}
		Expect(bc.validate()).To(MatchError(ContainSubstring(".domain")))
	})

	It("allows multiple capture groups (unlike partitionsConfig)", func() {
		bc := &batchPartitionsConfig{Name: "nvme-set", Matcher: `nvme_(.*)_(.*)`}
		Expect(bc.validate()).NotTo(HaveOccurred())
	})

	It("compiles matcher so it can be used after validate", func() {
		bc := &batchPartitionsConfig{Name: "nvme-set", Matcher: `nvme_data_\d+`}
		Expect(bc.validate()).NotTo(HaveOccurred())
		Expect(bc.matcher.MatchString("nvme_data_01")).To(BeTrue())
		Expect(bc.matcher.MatchString("sda1")).To(BeFalse())
	})
})

var _ = Describe("netBWConfig.validate", func() {
	It("accepts a valid matcher", func() {
		nc := &netBWConfig{Matcher: `eth.*`, MbpsPerShare: 100}
		Expect(nc.validate()).NotTo(HaveOccurred())
		Expect(nc.matcher).NotTo(BeNil())
	})

	It("rejects an invalid regexp", func() {
		nc := &netBWConfig{Matcher: `[`}
		Expect(nc.validate()).To(MatchError(ContainSubstring(".matcher")))
	})

	It("compiles matcher so it can be used after validate", func() {
		nc := &netBWConfig{Matcher: `eth\d+`}
		Expect(nc.validate()).NotTo(HaveOccurred())
		Expect(nc.matcher.MatchString("eth0")).To(BeTrue())
		Expect(nc.matcher.MatchString("wlan0")).To(BeFalse())
	})
})

var _ = Describe("netRdmaConfig.validate", func() {
	It("accepts a valid matcher", func() {
		nc := &netRdmaConfig{Matcher: `ib.*`, ResourceCount: 2}
		Expect(nc.validate()).NotTo(HaveOccurred())
		Expect(nc.matcher).NotTo(BeNil())
	})

	It("rejects an invalid regexp", func() {
		nc := &netRdmaConfig{Matcher: `[`}
		Expect(nc.validate()).To(MatchError(ContainSubstring(".matcher")))
	})
})

var _ = Describe("hostDevConfig.validate", func() {
	It("accepts a valid matcher", func() {
		hc := &hostDevConfig{Matcher: `/dev/sda.*`, Prefix: "sda"}
		Expect(hc.validate()).NotTo(HaveOccurred())
		Expect(hc.matcher).NotTo(BeNil())
	})

	It("rejects an invalid regexp", func() {
		hc := &hostDevConfig{Matcher: `[`}
		Expect(hc.validate()).To(MatchError(ContainSubstring(".matcher")))
	})
})

var _ = Describe("appConfig (full YAML round-trip)", func() {
	It("parses partitions section", func() {
		cfg := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
  - matcher: "ssd_(.*)"
    domain: storage.example.com
`)
		Expect(cfg.Partitions).To(HaveLen(2))
		Expect(cfg.Partitions[0].Matcher).To(Equal("nvme_(.*)"))
		Expect(cfg.Partitions[0].matcher).NotTo(BeNil())
		Expect(cfg.Partitions[1].DomainOverride).To(Equal("storage.example.com"))
	})

	It("parses batchPartitions section", func() {
		cfg := mustParseYAML(`
domain: ydb.tech
batchPartitions:
  - name: nvme-set
    matcher: "nvme_data_.*"
    count: 2
  - name: ssd-stripe
    matcher: "ssd_stripe_.*"
`)
		Expect(cfg.BatchPartitions).To(HaveLen(2))
		Expect(cfg.BatchPartitions[0].Name).To(Equal("nvme-set"))
		Expect(cfg.BatchPartitions[0].Count).To(Equal(2))
		Expect(cfg.BatchPartitions[0].matcher).NotTo(BeNil())
		Expect(cfg.BatchPartitions[1].Count).To(Equal(1)) // defaulted
	})

	It("parses networkBandwidth section", func() {
		cfg := mustParseYAML(`
domain: ydb.tech
networkBandwidth:
  - matcher: "eth(.*)"
    mbpsPerShare: 100
`)
		Expect(cfg.NetworkBandwidth).To(HaveLen(1))
		Expect(cfg.NetworkBandwidth[0].MbpsPerShare).To(BeEquivalentTo(100))
		Expect(cfg.NetworkBandwidth[0].matcher).NotTo(BeNil())
	})

	It("parses networkRdma section", func() {
		cfg := mustParseYAML(`
domain: ydb.tech
networkRdma:
  - matcher: "ib(.*)"
    resourceCount: 4
`)
		Expect(cfg.NetworkRdma).To(HaveLen(1))
		Expect(cfg.NetworkRdma[0].ResourceCount).To(BeEquivalentTo(4))
		Expect(cfg.NetworkRdma[0].matcher).NotTo(BeNil())
	})

	It("reports errors for each invalid section with its index", func() {
		_, err := parseYAML(`
domain: ydb.tech
partitions:
  - matcher: "["
batchPartitions:
  - name: ""
    matcher: "nvme.*"
networkBandwidth:
  - matcher: "["
networkRdma:
  - matcher: "["
`)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(".partitions[0]"))
		Expect(err.Error()).To(ContainSubstring(".batchPartitions[0]"))
		Expect(err.Error()).To(ContainSubstring(".networkBandwidth[0]"))
		Expect(err.Error()).To(ContainSubstring(".networkRdma[0]"))
	})
})
