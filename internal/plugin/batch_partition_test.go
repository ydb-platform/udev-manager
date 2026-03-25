package plugin

import (
	"context"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ydb-platform/udev-manager/internal/udev"
)

var _ = Describe("matchBatchPartitionDevice", func() {
	var matcher *regexp.Regexp

	BeforeEach(func() {
		matcher = regexp.MustCompile(`nvme.*`)
	})

	It("returns false for a non-block device", func() {
		dev := netDevice("eth0", "1000", "up")
		_, _, ok := matchBatchPartitionDevice(dev, matcher)
		Expect(ok).To(BeFalse())
	})

	It("returns false for a block disk (not a partition)", func() {
		dev := &mockDevice{
			id:         udev.Id("sda"),
			subsystem:  udev.BlockSubsystem,
			devType:    "disk",
			properties: map[string]string{},
		}
		_, _, ok := matchBatchPartitionDevice(dev, matcher)
		Expect(ok).To(BeFalse())
	})

	It("returns false for a partition without a PARTNAME property", func() {
		dev := &mockDevice{
			id:         udev.Id("sda1"),
			subsystem:  udev.BlockSubsystem,
			devType:    udev.DeviceTypePart,
			properties: map[string]string{},
		}
		_, _, ok := matchBatchPartitionDevice(dev, matcher)
		Expect(ok).To(BeFalse())
	})

	It("returns false when the label does not match the regexp", func() {
		dev := partitionDevice("sda1", "data_01")
		_, _, ok := matchBatchPartitionDevice(dev, matcher)
		Expect(ok).To(BeFalse())
	})

	It("returns the full match as label when there is no capture group", func() {
		dev := partitionDevice("nvme0n1p1", "nvme_data_01")
		id, label, ok := matchBatchPartitionDevice(dev, matcher)
		Expect(ok).To(BeTrue())
		Expect(id).To(Equal(udev.Id("nvme0n1p1")))
		Expect(label).To(Equal("nvme_data_01"))
	})

	It("returns capture group 1 as label when a capture group is present", func() {
		m := regexp.MustCompile(`nvme_(.*)`)
		dev := partitionDevice("nvme0n1p1", "nvme_data_01")
		id, label, ok := matchBatchPartitionDevice(dev, m)
		Expect(ok).To(BeTrue())
		Expect(id).To(Equal(udev.Id("nvme0n1p1")))
		Expect(label).To(Equal("data_01"))
	})
})

var _ = Describe("batchPartitionPool", func() {
	var pool *batchPartitionPool
	var dev1, dev2 *mockDevice

	BeforeEach(func() {
		pool = &batchPartitionPool{
			parts:  make(map[udev.Id]udev.Device),
			labels: make(map[udev.Id]string),
			domain: "ydb.tech",
		}
		dev1 = partitionDevice("nvme0n1p1", "nvme_data_01")
		dev1.sysattrs = map[string]string{
			udev.SysAttrWWID:  "wwid1",
			udev.SysAttrModel: "NVMe SSD",
		}
		dev2 = partitionDevice("nvme1n1p1", "nvme_data_02")
		dev2.sysattrs = map[string]string{
			udev.SysAttrWWID:  "wwid2",
			udev.SysAttrModel: "NVMe SSD",
		}
	})

	Describe("health", func() {
		It("returns Unhealthy when the pool is empty", func() {
			Expect(pool.health()).To(BeAssignableToTypeOf(Unhealthy{}))
		})

		It("returns Healthy when the pool has at least one device", func() {
			pool.add(dev1, "nvme_data_01")
			Expect(pool.health()).To(BeAssignableToTypeOf(Healthy{}))
		})
	})

	Describe("empty", func() {
		It("returns true when the pool is empty", func() {
			Expect(pool.empty()).To(BeTrue())
		})

		It("returns false after a device is added", func() {
			pool.add(dev1, "nvme_data_01")
			Expect(pool.empty()).To(BeFalse())
		})

		It("returns true again after the last device is removed", func() {
			pool.add(dev1, "nvme_data_01")
			pool.remove(dev1.Id())
			Expect(pool.empty()).To(BeTrue())
		})
	})

	Describe("add and remove", func() {
		It("increases pool size on add", func() {
			pool.add(dev1, "nvme_data_01")
			Expect(pool.parts).To(HaveLen(1))
		})

		It("adding the same device twice is idempotent", func() {
			pool.add(dev1, "nvme_data_01")
			pool.add(dev1, "nvme_data_01")
			Expect(pool.parts).To(HaveLen(1))
		})

		It("removes only the specified device", func() {
			pool.add(dev1, "nvme_data_01")
			pool.add(dev2, "nvme_data_02")
			pool.remove(dev1.Id())
			Expect(pool.parts).To(HaveLen(1))
			Expect(pool.parts).To(HaveKey(dev2.Id()))
		})
	})

	Describe("allocate", func() {
		It("returns an empty response for an empty pool", func() {
			resp, err := pool.allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Devices).To(BeEmpty())
			Expect(resp.Envs).To(BeEmpty())
		})

		It("returns one device spec for a single device in the pool", func() {
			pool.add(dev1, "nvme_data_01")
			resp, err := pool.allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Devices).To(HaveLen(1))
			Expect(resp.Devices[0].HostPath).To(Equal("/dev/nvme0n1p1"))
			Expect(resp.Devices[0].ContainerPath).To(Equal("/dev/allocated/ydb.tech/part/nvme_data_01"))
		})

		It("returns merged device specs for multiple devices", func() {
			pool.add(dev1, "nvme_data_01")
			pool.add(dev2, "nvme_data_02")
			resp, err := pool.allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Devices).To(HaveLen(2))
		})

		It("sets env vars for each partition using the partition label", func() {
			pool.add(dev1, "nvme_data_01")
			resp, err := pool.allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Envs).To(HaveKey("YDB_TECH_PART_NVME_DATA_01_PATH"))
		})
	})
})

var _ = Describe("batchPartitionSeat", func() {
	var pool *batchPartitionPool
	var seat *batchPartitionSeat

	BeforeEach(func() {
		pool = &batchPartitionPool{
			parts:  make(map[udev.Id]udev.Device),
			labels: make(map[udev.Id]string),
			domain: "ydb.tech",
		}
		seat = &batchPartitionSeat{id: "0", pool: pool}
	})

	It("returns the configured Id", func() {
		Expect(seat.Id()).To(Equal(Id("0")))
	})

	It("is Unhealthy when the pool is empty", func() {
		Expect(seat.Health()).To(BeAssignableToTypeOf(Unhealthy{}))
	})

	It("is Healthy when the pool has a device", func() {
		pool.add(partitionDevice("nvme0n1p1", "nvme_data"), "nvme_data")
		Expect(seat.Health()).To(BeAssignableToTypeOf(Healthy{}))
	})

	It("returns nil TopologyHints", func() {
		Expect(seat.TopologyHints()).To(BeNil())
	})

	It("delegates Allocate to the pool", func() {
		pool.add(partitionDevice("nvme0n1p1", "nvme_data"), "nvme_data")
		resp, err := seat.Allocate(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Devices).To(HaveLen(1))
	})
})

var _ = Describe("newResource Close", func() {
	It("stops the run goroutine and closes instanceCh", func() {
		// Simulates what happens when registry.Add fails and res.Close() is called:
		// the eagerly-started goroutine must exit cleanly.
		res := newResource(
			ResourceTemplate{Domain: "ydb.tech", Prefix: "batch-test"},
			map[Id]Instance{},
		)
		ch := res.ListAndWatch(context.Background())
		Eventually(ch).Should(Receive()) // drain initial snapshot

		res.Close()
		Eventually(ch).Should(BeClosed())
	})
})

var _ = Describe("runBatchPartitionScatter", func() {
	var (
		pool    *batchPartitionPool
		res     *resource
		seats   []Instance
		evCh    chan udev.Event
		matcher *regexp.Regexp
		watchCh <-chan []Instance
	)

	BeforeEach(func() {
		matcher = regexp.MustCompile(`nvme.*`)
		pool = &batchPartitionPool{
			parts:  make(map[udev.Id]udev.Device),
			labels: make(map[udev.Id]string),
			domain: "ydb.tech",
		}
		seat := &batchPartitionSeat{id: "0", pool: pool}
		seats = []Instance{seat}
		res = newResource(
			ResourceTemplate{Domain: "ydb.tech", Prefix: "batch-nvme"},
			map[Id]Instance{seat.Id(): seat},
		)
		DeferCleanup(res.Close)

		watchCh = res.ListAndWatch(context.Background())
		Eventually(watchCh).Should(Receive()) // drain initial snapshot

		evCh = make(chan udev.Event, 10)
		go runBatchPartitionScatter(evCh, pool, matcher, res, seats)
	})

	AfterEach(func() {
		close(evCh)
	})

	Describe("Init event", func() {
		It("populates the pool with all matching devices", func() {
			dev1 := partitionDevice("nvme0n1p1", "nvme_data_01")
			dev2 := partitionDevice("nvme1n1p1", "nvme_data_02")
			dev3 := partitionDevice("sda1", "data_01") // non-matching
			evCh <- udev.Init{Devices: []udev.Device{dev1, dev2, dev3}}

			Eventually(func() int {
				pool.mu.RLock()
				defer pool.mu.RUnlock()
				return len(pool.parts)
			}).Should(Equal(2))
		})

		It("emits a health event when matching devices are found", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_data_01")
			evCh <- udev.Init{Devices: []udev.Device{dev}}
			Eventually(watchCh).Should(Receive())
		})

		It("does not emit a health event when no matching devices are found", func() {
			dev := partitionDevice("sda1", "data_01")
			evCh <- udev.Init{Devices: []udev.Device{dev}}
			Consistently(watchCh, 50*time.Millisecond).ShouldNot(Receive())
		})

		It("emits only one health event for multiple matching devices in Init", func() {
			dev1 := partitionDevice("nvme0n1p1", "nvme_data_01")
			dev2 := partitionDevice("nvme1n1p1", "nvme_data_02")
			evCh <- udev.Init{Devices: []udev.Device{dev1, dev2}}
			Eventually(watchCh).Should(Receive())
			Consistently(watchCh, 50*time.Millisecond).ShouldNot(Receive())
		})
	})

	Describe("Added event", func() {
		It("adds a matching device to the pool", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_data_01")
			evCh <- udev.Added{Device: dev}
			Eventually(func() bool { return !pool.empty() }).Should(BeTrue())
		})

		It("emits a health event on the empty-to-non-empty transition", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_data_01")
			evCh <- udev.Added{Device: dev}
			Eventually(watchCh).Should(Receive())
		})

		It("does not emit a health event when the pool was already non-empty", func() {
			dev1 := partitionDevice("nvme0n1p1", "nvme_data_01")
			dev2 := partitionDevice("nvme1n1p1", "nvme_data_02")
			evCh <- udev.Added{Device: dev1}
			Eventually(watchCh).Should(Receive()) // drain the first transition

			evCh <- udev.Added{Device: dev2}
			Consistently(watchCh, 50*time.Millisecond).ShouldNot(Receive())
		})

		It("ignores non-matching devices", func() {
			dev := partitionDevice("sda1", "data_01")
			evCh <- udev.Added{Device: dev}
			Consistently(func() bool { return pool.empty() }, 50*time.Millisecond).Should(BeTrue())
			Consistently(watchCh, 50*time.Millisecond).ShouldNot(Receive())
		})
	})

	Describe("Removed event", func() {
		BeforeEach(func() {
			// Seed the pool with one matching device.
			dev := partitionDevice("nvme0n1p1", "nvme_data_01")
			evCh <- udev.Added{Device: dev}
			Eventually(watchCh).Should(Receive()) // drain the Healthy event
		})

		It("removes a matching device from the pool", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_data_01")
			evCh <- udev.Removed{Device: dev}
			Eventually(func() bool { return pool.empty() }).Should(BeTrue())
		})

		It("emits a health event when the pool becomes empty", func() {
			dev := partitionDevice("nvme0n1p1", "nvme_data_01")
			evCh <- udev.Removed{Device: dev}
			Eventually(watchCh).Should(Receive())
		})

		It("does not emit a health event when the pool still has devices", func() {
			dev2 := partitionDevice("nvme1n1p1", "nvme_data_02")
			evCh <- udev.Added{Device: dev2}
			// pool: dev1 + dev2; no health event (was already non-empty)
			Consistently(watchCh, 50*time.Millisecond).ShouldNot(Receive())

			// Remove dev1; pool still has dev2, so no event
			dev1 := partitionDevice("nvme0n1p1", "nvme_data_01")
			evCh <- udev.Removed{Device: dev1}
			Consistently(watchCh, 50*time.Millisecond).ShouldNot(Receive())
		})

		It("ignores Removed events for non-matching devices", func() {
			dev := partitionDevice("sda1", "data_01")
			evCh <- udev.Removed{Device: dev}
			Consistently(watchCh, 50*time.Millisecond).ShouldNot(Receive())
		})
	})
})
