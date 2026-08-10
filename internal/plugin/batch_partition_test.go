package plugin

import (
	"context"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ydb-platform/udev-manager/internal/udev"
)

func batchPartitionPoolForTest(domain string, devices ...udev.Device) *batchPartitionPool {
	parts, labels := batchPartitionState(
		udev.Snapshot{Devices: devices},
		regexp.MustCompile(`.*`),
	)
	return &batchPartitionPool{
		parts:  parts,
		labels: labels,
		domain: domain,
	}
}

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
		pool = batchPartitionPoolForTest("ydb.tech")
	})

	Describe("health", func() {
		It("returns Unhealthy when the pool is empty", func() {
			Expect(pool.health()).To(BeAssignableToTypeOf(Unhealthy{}))
		})

		It("returns Healthy when the pool has at least one device", func() {
			pool = batchPartitionPoolForTest("ydb.tech", dev1)
			Expect(pool.health()).To(BeAssignableToTypeOf(Healthy{}))
		})
	})

	Describe("empty", func() {
		It("returns true when the pool is empty", func() {
			Expect(pool.empty()).To(BeTrue())
		})

		It("returns false for a generation with a device", func() {
			pool = batchPartitionPoolForTest("ydb.tech", dev1)
			Expect(pool.empty()).To(BeFalse())
		})
	})

	Describe("generation isolation", func() {
		It("does not mutate an older pool when a new one is built", func() {
			oldPool := batchPartitionPoolForTest("ydb.tech", dev1)
			newPool := batchPartitionPoolForTest("ydb.tech", dev2)

			Expect(oldPool.parts).To(HaveKey(dev1.Id()))
			Expect(oldPool.parts).NotTo(HaveKey(dev2.Id()))
			Expect(newPool.parts).To(HaveKey(dev2.Id()))
			Expect(newPool.parts).NotTo(HaveKey(dev1.Id()))
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
			pool = batchPartitionPoolForTest("ydb.tech", dev1)
			resp, err := pool.allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Devices).To(HaveLen(1))
			Expect(resp.Devices[0].HostPath).To(Equal("/dev/nvme0n1p1"))
			Expect(resp.Devices[0].ContainerPath).To(Equal("/dev/allocated/ydb.tech/part/nvme_data_01"))
		})

		It("returns merged device specs for multiple devices", func() {
			pool = batchPartitionPoolForTest("ydb.tech", dev1, dev2)
			resp, err := pool.allocate(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Devices).To(HaveLen(2))
		})

		It("sets env vars for each partition using the partition label", func() {
			pool = batchPartitionPoolForTest("ydb.tech", dev1)
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
		pool = batchPartitionPoolForTest("ydb.tech")
		seat = &batchPartitionSeat{id: "0", pool: pool}
	})

	It("returns the configured Id", func() {
		Expect(seat.Id()).To(Equal(Id("0")))
	})

	It("is Unhealthy when the pool is empty", func() {
		Expect(seat.Health()).To(BeAssignableToTypeOf(Unhealthy{}))
	})

	It("is Healthy when the pool has a device", func() {
		seat.pool = batchPartitionPoolForTest(
			"ydb.tech",
			partitionDevice("nvme0n1p1", "nvme_data"),
		)
		Expect(seat.Health()).To(BeAssignableToTypeOf(Healthy{}))
	})

	It("returns nil TopologyHints", func() {
		Expect(seat.TopologyHints()).To(BeNil())
	})

	It("delegates Allocate to the pool", func() {
		seat.pool = batchPartitionPoolForTest(
			"ydb.tech",
			partitionDevice("nvme0n1p1", "nvme_data"),
		)
		resp, err := seat.Allocate(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Devices).To(HaveLen(1))
	})
})

var _ = Describe("newResource Close", func() {
	It("closes resource watchers", func() {
		res := newResource(
			ResourceTemplate{Domain: "ydb.tech", Prefix: "batch-test"},
			map[Id]Instance{},
		)
		updates := res.Watch(context.Background())
		Eventually(updates).Should(Receive()) // initial token

		res.Close()
		Eventually(updates).Should(BeClosed())
	})
})

var _ = Describe("runBatchPartitionScatter", func() {
	var (
		res        *resource
		snapshotCh chan udev.Snapshot
		matcher    *regexp.Regexp
		updates    <-chan struct{}
		runDone    chan struct{}
	)

	BeforeEach(func() {
		matcher = regexp.MustCompile(`nvme.*`)
		instances := batchPartitionInstances(udev.Snapshot{}, matcher, "ydb.tech", 1)
		res = newResource(
			ResourceTemplate{Domain: "ydb.tech", Prefix: "batch-nvme"},
			instances,
		)
		DeferCleanup(res.Close)

		updates = res.Watch(context.Background())
		Eventually(updates).Should(Receive()) // initial token

		snapshotCh = make(chan udev.Snapshot, 10)
		runDone = make(chan struct{})
		go func() {
			defer close(runDone)
			runBatchPartitionScatter(snapshotCh, matcher, "ydb.tech", 1, res)
		}()
	})

	AfterEach(func() {
		close(snapshotCh)
		Eventually(runDone).Should(BeClosed())
	})

	It("atomically replaces the pool with all matching devices", func() {
		dev1 := partitionDevice("nvme0n1p1", "nvme_data_01")
		dev2 := partitionDevice("nvme1n1p1", "nvme_data_02")
		dev3 := partitionDevice("sda1", "data_01")
		snapshotCh <- udev.Snapshot{Generation: 1, Devices: []udev.Device{dev1, dev2, dev3}}

		Eventually(func() int {
			seat := res.Instances()[Id("0")].(*batchPartitionSeat)
			return len(seat.pool.parts)
		}).Should(Equal(2))
		Eventually(updates).Should(Receive())
		Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("swaps fresh seats without mutating the previous healthy generation", func() {
		dev1 := partitionDevice("nvme0n1p1", "nvme_data_01")
		snapshotCh <- udev.Snapshot{Generation: 1, Devices: []udev.Device{dev1}}
		Eventually(updates).Should(Receive())
		oldSeat := res.Instances()[Id("0")].(*batchPartitionSeat)

		dev2 := partitionDevice("nvme1n1p1", "nvme_data_02")
		snapshotCh <- udev.Snapshot{Generation: 2, Devices: []udev.Device{dev2}}
		var newSeat *batchPartitionSeat
		Eventually(func() bool {
			newSeat = res.Instances()[Id("0")].(*batchPartitionSeat)
			return newSeat != oldSeat
		}).Should(BeTrue())

		Expect(oldSeat.pool).NotTo(BeIdenticalTo(newSeat.pool))
		Expect(oldSeat.pool.parts).To(HaveKey(dev1.Id()))
		Expect(oldSeat.pool.parts).NotTo(HaveKey(dev2.Id()))
		Expect(newSeat.pool.parts).To(HaveKey(dev2.Id()))
		Expect(newSeat.pool.parts).NotTo(HaveKey(dev1.Id()))

		oldResponse, err := oldSeat.Allocate(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(oldResponse.Devices).To(ConsistOf(HaveField("HostPath", "/dev/nvme0n1p1")))
		newResponse, err := newSeat.Allocate(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(newResponse.Devices).To(ConsistOf(HaveField("HostPath", "/dev/nvme1n1p1")))
		Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("does not notify while a fresh generation remains healthy", func() {
		dev1 := partitionDevice("nvme0n1p1", "nvme_data_01")
		dev2 := partitionDevice("nvme1n1p1", "nvme_data_02")
		snapshotCh <- udev.Snapshot{Generation: 1, Devices: []udev.Device{dev1}}
		Eventually(updates).Should(Receive())
		oldSeat := res.Instances()[Id("0")].(*batchPartitionSeat)

		snapshotCh <- udev.Snapshot{Generation: 2, Devices: []udev.Device{dev1, dev2}}
		var newSeat *batchPartitionSeat
		Eventually(func() bool {
			newSeat = res.Instances()[Id("0")].(*batchPartitionSeat)
			return newSeat != oldSeat && len(newSeat.pool.parts) == 2
		}).Should(BeTrue())
		Expect(oldSeat.pool.parts).To(HaveLen(1))
		Consistently(updates, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("publishes an empty generation without changing an in-flight old seat", func() {
		dev := partitionDevice("nvme0n1p1", "nvme_data_01")
		snapshotCh <- udev.Snapshot{Generation: 1, Devices: []udev.Device{dev}}
		Eventually(updates).Should(Receive())
		oldSeat := res.Instances()[Id("0")].(*batchPartitionSeat)

		snapshotCh <- udev.Snapshot{Generation: 2}
		Eventually(updates).Should(Receive())
		newSeat := res.Instances()[Id("0")].(*batchPartitionSeat)
		Expect(newSeat).NotTo(BeIdenticalTo(oldSeat))
		Expect(newSeat.pool).NotTo(BeIdenticalTo(oldSeat.pool))
		Expect(newSeat.pool.empty()).To(BeTrue())
		Expect(res.Devices()).To(ConsistOf(HaveField("Health", "Unhealthy")))

		Expect(oldSeat.Health()).To(BeAssignableToTypeOf(Healthy{}))
		oldResponse, err := oldSeat.Allocate(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(oldResponse.Devices).To(ConsistOf(HaveField("HostPath", "/dev/nvme0n1p1")))
	})
})
