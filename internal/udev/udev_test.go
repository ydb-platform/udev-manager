package udev_test

import (
	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// blockDevice is a convenience builder for a simple partition-style device.
func blockPartition(id, label string) *udev.FakeDevice {
	return udev.NewFakeDevice(udev.Id(id)).
		WithSubsystem(udev.BlockSubsystem).
		WithDevType(udev.DeviceTypePart).
		WithDevNode("/dev/"+id).
		WithProperty(udev.PropertyPartName, label)
}

// isBlock is a filter that matches block partition devices.
func isBlock(dev udev.Device) bool {
	return dev.Subsystem() == udev.BlockSubsystem && dev.DevType() == udev.DeviceTypePart
}

// ---------------------------------------------------------------------------
// FakeDevice
// ---------------------------------------------------------------------------

var _ = Describe("FakeDevice", func() {
	var dev *udev.FakeDevice

	BeforeEach(func() {
		dev = udev.NewFakeDevice("sysfs/nvme0n1p1")
	})

	It("returns the configured ID", func() {
		Expect(dev.Id()).To(Equal(udev.Id("sysfs/nvme0n1p1")))
	})

	It("defaults NumaNode to -1", func() {
		Expect(dev.NumaNode()).To(Equal(-1))
	})

	It("returns configured scalar fields", func() {
		dev.WithSubsystem("block").WithDevType("partition").WithDevNode("/dev/nvme0n1p1").WithNumaNode(2)
		Expect(dev.Subsystem()).To(Equal("block"))
		Expect(dev.DevType()).To(Equal("partition"))
		Expect(dev.DevNode()).To(Equal("/dev/nvme0n1p1"))
		Expect(dev.NumaNode()).To(Equal(2))
	})

	It("returns configured properties", func() {
		dev.WithProperty("PARTNAME", "data").WithProperty("ID_MODEL", "NVMe")
		Expect(dev.Property("PARTNAME")).To(Equal("data"))
		Expect(dev.Property("ID_MODEL")).To(Equal("NVMe"))
		Expect(dev.Property("MISSING")).To(BeEmpty())
		Expect(dev.Properties()).To(HaveKey("PARTNAME"))
	})

	It("returns configured sysattrs", func() {
		dev.WithSysAttr("speed", "25000").WithSysAttr("operstate", "up")
		Expect(dev.SystemAttribute("speed")).To(Equal("25000"))
		Expect(dev.SystemAttribute("operstate")).To(Equal("up"))
		Expect(dev.SystemAttribute("missing")).To(BeEmpty())
		Expect(dev.SystemAttributes()).To(HaveKey("speed"))
		Expect(dev.SystemAttributeKeys()).To(ContainElement("speed"))
	})

	It("returns configured tags and devlinks", func() {
		dev.WithTags("seat", "uaccess").WithDevLinks("/dev/disk/by-id/nvme0")
		Expect(dev.Tags()).To(ConsistOf("seat", "uaccess"))
		Expect(dev.DevLinks()).To(ConsistOf("/dev/disk/by-id/nvme0"))
	})

	It("returns nil for Parent when not configured", func() {
		Expect(dev.Parent()).To(BeNil())
	})

	It("returns the configured parent", func() {
		parent := udev.NewFakeDevice("sysfs/nvme0")
		dev.WithParent(parent)
		Expect(dev.Parent()).To(Equal(parent))
	})

	It("Debug returns a non-empty string containing the ID", func() {
		Expect(dev.Debug()).To(ContainSubstring("sysfs/nvme0n1p1"))
	})

	Describe("PropertyLookup", func() {
		It("returns own value when present", func() {
			dev.WithProperty("KEY", "own")
			Expect(dev.PropertyLookup("KEY")).To(Equal("own"))
		})

		It("returns empty string when absent and no parent", func() {
			Expect(dev.PropertyLookup("MISSING")).To(BeEmpty())
		})

		It("falls back to parent when own value is absent", func() {
			parent := udev.NewFakeDevice("parent").WithProperty("KEY", "from-parent")
			dev.WithParent(parent)
			Expect(dev.PropertyLookup("KEY")).To(Equal("from-parent"))
		})

		It("own value takes priority over parent", func() {
			parent := udev.NewFakeDevice("parent").WithProperty("KEY", "from-parent")
			dev.WithProperty("KEY", "mine").WithParent(parent)
			Expect(dev.PropertyLookup("KEY")).To(Equal("mine"))
		})

		It("walks the full ancestor chain", func() {
			grandparent := udev.NewFakeDevice("gp").WithProperty("KEY", "gp-value")
			parent := udev.NewFakeDevice("p").WithParent(grandparent)
			dev.WithParent(parent)
			Expect(dev.PropertyLookup("KEY")).To(Equal("gp-value"))
		})
	})

	Describe("SystemAttributeLookup", func() {
		It("returns own attr when present", func() {
			dev.WithSysAttr("numa_node", "1")
			Expect(dev.SystemAttributeLookup("numa_node")).To(Equal("1"))
		})

		It("returns empty string when absent and no parent", func() {
			Expect(dev.SystemAttributeLookup("missing")).To(BeEmpty())
		})

		It("falls back to parent", func() {
			parent := udev.NewFakeDevice("p").WithSysAttr("numa_node", "3")
			dev.WithParent(parent)
			Expect(dev.SystemAttributeLookup("numa_node")).To(Equal("3"))
		})

		It("own attr takes priority over parent", func() {
			parent := udev.NewFakeDevice("p").WithSysAttr("numa_node", "3")
			dev.WithSysAttr("numa_node", "0").WithParent(parent)
			Expect(dev.SystemAttributeLookup("numa_node")).To(Equal("0"))
		})
	})
})

// ---------------------------------------------------------------------------
// FakeDiscovery — parent linkage through Init
// ---------------------------------------------------------------------------

var _ = Describe("FakeDiscovery parent linkage", func() {
	var d *udev.FakeDiscovery

	BeforeEach(func() {
		d = udev.NewFakeDiscovery()
	})

	AfterEach(func() {
		d.Close()
	})

	// In production, udev_enumerate_scan_devices returns devices sorted in
	// dependency order (parents before children). The enumeration loop in
	// monitor() relies on this to resolve parent references from d.state.
	// This test verifies that the parent relationship survives Init delivery.
	It("delivers child devices with their parent reference intact", func() {
		parent := udev.NewFakeDevice("sysfs/nvme0").
			WithSubsystem(udev.BlockSubsystem).
			WithDevType("disk").
			WithDevNode("/dev/nvme0")
		child := udev.NewFakeDevice("sysfs/nvme0n1p1").
			WithSubsystem(udev.BlockSubsystem).
			WithDevType(udev.DeviceTypePart).
			WithDevNode("/dev/nvme0n1p1").
			WithParent(parent)

		// Add parent first, then child — mirrors libudev dependency order.
		d.AddDevice(parent)
		d.AddDevice(child)

		ch := make(chan udev.Event, 4)
		cancel := d.Subscribe(mux.SinkFromChan(ch))
		defer cancel()

		var initEvt udev.Event
		Eventually(ch).Should(Receive(&initEvt))

		devices := initEvt.(udev.Init).Devices
		Expect(devices).To(HaveLen(2))

		// Find the child in the Init snapshot and verify its parent.
		var found udev.Device
		for _, dev := range devices {
			if dev.Id() == "sysfs/nvme0n1p1" {
				found = dev
			}
		}
		Expect(found).NotTo(BeNil())
		Expect(found.Parent()).NotTo(BeNil())
		Expect(found.Parent().Id()).To(Equal(udev.Id("sysfs/nvme0")))
	})

	It("child Parent() returns nil when parent was not added", func() {
		// Simulates a broken dependency order or missing parent.
		child := udev.NewFakeDevice("sysfs/nvme0n1p1").
			WithSubsystem(udev.BlockSubsystem).
			WithDevType(udev.DeviceTypePart)

		d.AddDevice(child)

		ch := make(chan udev.Event, 4)
		cancel := d.Subscribe(mux.SinkFromChan(ch))
		defer cancel()

		var initEvt udev.Event
		Eventually(ch).Should(Receive(&initEvt))

		devices := initEvt.(udev.Init).Devices
		Expect(devices).To(HaveLen(1))
		Expect(devices[0].Parent()).To(BeNil())
	})

	// In production, generic.Parent() lazily resolves via DeviceById when
	// the eager parent assignment during enumeration yields nil (e.g. if a
	// child were enumerated before its parent). This test verifies that
	// DeviceById returns the parent once it has been added, which is the
	// contract the lazy resolution depends on.
	It("resolves parent via DeviceById when child is added first", func() {
		parent := udev.NewFakeDevice("sysfs/nvme0").
			WithSubsystem(udev.BlockSubsystem).
			WithDevType("disk").
			WithDevNode("/dev/nvme0")
		child := udev.NewFakeDevice("sysfs/nvme0n1p1").
			WithSubsystem(udev.BlockSubsystem).
			WithDevType(udev.DeviceTypePart).
			WithDevNode("/dev/nvme0n1p1")

		// Add child first — parent is not yet in state.
		d.AddDevice(child)
		Expect(d.DeviceById("sysfs/nvme0")).To(BeNil())

		// Add parent afterward.
		d.AddDevice(parent)
		Expect(d.DeviceById("sysfs/nvme0")).To(Equal(parent))
	})
})

// ---------------------------------------------------------------------------
// FakeDiscovery — Subscribe
// ---------------------------------------------------------------------------

var _ = Describe("FakeDiscovery Subscribe", func() {
	var d *udev.FakeDiscovery

	BeforeEach(func() {
		d = udev.NewFakeDiscovery()
	})

	AfterEach(func() {
		d.Close()
	})

	It("immediately delivers Init with an empty device list when state is empty", func() {
		ch := make(chan udev.Event, 4)
		cancel := d.Subscribe(mux.SinkFromChan(ch))
		defer cancel()

		Eventually(ch).Should(Receive(Equal(udev.Init{Devices: []udev.Device{}})))
	})

	It("immediately delivers Init carrying pre-populated devices", func() {
		dev1 := blockPartition("nvme0n1p1", "data")
		dev2 := blockPartition("nvme0n1p2", "log")
		d.AddDevice(dev1)
		d.AddDevice(dev2)

		ch := make(chan udev.Event, 4)
		cancel := d.Subscribe(mux.SinkFromChan(ch))
		defer cancel()

		Eventually(ch).Should(Receive(WithTransform(
			func(ev udev.Event) []udev.Device { return ev.(udev.Init).Devices },
			ConsistOf(dev1, dev2),
		)))
	})

	It("delivers Added events after the initial Init", func() {
		ch := make(chan udev.Event, 4)
		cancel := d.Subscribe(mux.SinkFromChan(ch))
		defer cancel()

		// Drain Init.
		Eventually(ch).Should(Receive(BeAssignableToTypeOf(udev.Init{})))

		dev := blockPartition("nvme0n1p1", "data")
		d.Emit(udev.Added{Device: dev})

		Eventually(ch).Should(Receive(Equal(udev.Added{Device: dev})))
	})

	It("delivers Removed events", func() {
		dev := blockPartition("nvme0n1p1", "data")
		d.AddDevice(dev)

		ch := make(chan udev.Event, 4)
		cancel := d.Subscribe(mux.SinkFromChan(ch))
		defer cancel()

		// Drain Init.
		Eventually(ch).Should(Receive(BeAssignableToTypeOf(udev.Init{})))

		d.Emit(udev.Removed{Device: dev})
		Eventually(ch).Should(Receive(Equal(udev.Removed{Device: dev})))
	})

	It("stops delivering events after CancelFunc is called", func() {
		ch := make(chan udev.Event, 4)
		cancel := d.Subscribe(mux.SinkFromChan(ch))

		// Drain Init.
		Eventually(ch).Should(Receive(BeAssignableToTypeOf(udev.Init{})))

		cancel()

		// Use a second subscriber as a delivery barrier so we know the Emit
		// has been fully processed before we assert the first sink is silent.
		barrier := make(chan udev.Event, 4)
		barrierCancel := d.Subscribe(mux.SinkFromChan(barrier))
		defer barrierCancel()
		Eventually(barrier).Should(Receive(BeAssignableToTypeOf(udev.Init{})))

		dev := blockPartition("nvme0n1p1", "data")
		d.Emit(udev.Added{Device: dev})
		Eventually(barrier).Should(Receive(BeAssignableToTypeOf(udev.Added{})))

		Expect(ch).NotTo(Receive())
	})

	It("delivers to multiple independent subscribers", func() {
		ch1 := make(chan udev.Event, 4)
		ch2 := make(chan udev.Event, 4)
		cancel1 := d.Subscribe(mux.SinkFromChan(ch1))
		cancel2 := d.Subscribe(mux.SinkFromChan(ch2))
		defer cancel1()
		defer cancel2()

		dev := blockPartition("nvme0n1p1", "data")
		d.Emit(udev.Added{Device: dev})

		Eventually(ch1).Should(Receive(BeAssignableToTypeOf(udev.Added{})))
		Eventually(ch2).Should(Receive(BeAssignableToTypeOf(udev.Added{})))
	})
})

// ---------------------------------------------------------------------------
// FakeDiscovery — State
// ---------------------------------------------------------------------------

var _ = Describe("FakeDiscovery State", func() {
	var d *udev.FakeDiscovery

	BeforeEach(func() {
		d = udev.NewFakeDiscovery()
	})

	AfterEach(func() {
		d.Close()
	})

	It("returns an empty map when no devices are present", func() {
		Expect(d.State(mux.Any[udev.Device]())).To(BeEmpty())
	})

	It("returns all devices with an Any() filter", func() {
		dev1 := blockPartition("nvme0n1p1", "data")
		dev2 := blockPartition("nvme0n1p2", "log")
		d.AddDevice(dev1)
		d.AddDevice(dev2)

		state := d.State(mux.Any[udev.Device]())
		Expect(state).To(HaveLen(2))
		Expect(state).To(HaveKey(udev.Id("nvme0n1p1")))
		Expect(state).To(HaveKey(udev.Id("nvme0n1p2")))
	})

	It("applies the filter predicate", func() {
		block := blockPartition("nvme0n1p1", "data")
		net := udev.NewFakeDevice("eth0").WithSubsystem(udev.NetSubsystem)
		d.AddDevice(block)
		d.AddDevice(net)

		state := d.State(isBlock)
		Expect(state).To(HaveLen(1))
		Expect(state).To(HaveKey(udev.Id("nvme0n1p1")))
	})

	It("reflects Emit(Added) immediately", func() {
		dev := blockPartition("nvme0n1p1", "data")
		d.Emit(udev.Added{Device: dev})

		Expect(d.State(mux.Any[udev.Device]())).To(HaveKey(udev.Id("nvme0n1p1")))
	})

	It("reflects Emit(Removed) immediately", func() {
		dev := blockPartition("nvme0n1p1", "data")
		d.AddDevice(dev)
		d.Emit(udev.Removed{Device: dev})

		Expect(d.State(mux.Any[udev.Device]())).To(BeEmpty())
	})
})

// ---------------------------------------------------------------------------
// FakeDiscovery — DeviceById
// ---------------------------------------------------------------------------

var _ = Describe("FakeDiscovery DeviceById", func() {
	var d *udev.FakeDiscovery

	BeforeEach(func() {
		d = udev.NewFakeDiscovery()
	})

	AfterEach(func() {
		d.Close()
	})

	It("returns nil for an unknown ID", func() {
		Expect(d.DeviceById("unknown")).To(BeNil())
	})

	It("returns a device added with AddDevice", func() {
		dev := blockPartition("nvme0n1p1", "data")
		d.AddDevice(dev)
		Expect(d.DeviceById("nvme0n1p1")).To(Equal(dev))
	})

	It("returns a device added via Emit(Added)", func() {
		dev := blockPartition("nvme0n1p1", "data")
		d.Emit(udev.Added{Device: dev})
		Expect(d.DeviceById("nvme0n1p1")).To(Equal(dev))
	})

	It("returns nil after the device is removed", func() {
		dev := blockPartition("nvme0n1p1", "data")
		d.AddDevice(dev)
		d.Emit(udev.Removed{Device: dev})
		Expect(d.DeviceById("nvme0n1p1")).To(BeNil())
	})
})

// ---------------------------------------------------------------------------
// Slice
// ---------------------------------------------------------------------------

var _ = Describe("Slice", func() {
	var d *udev.FakeDiscovery

	BeforeEach(func() {
		d = udev.NewFakeDiscovery()
	})

	AfterEach(func() {
		d.Close()
	})

	// subscribeSlice returns a channel that receives successive device-set
	// snapshots emitted by the slice, plus a cleanup func.
	subscribeSlice := func(s udev.Slice) (<-chan []udev.Device, func()) {
		ch := make(chan []udev.Device, 8)
		cancel := s.Subscribe(mux.SinkFromChan(ch))
		return ch, cancel
	}

	It("emits the current matching devices from Init", func() {
		dev1 := blockPartition("nvme0n1p1", "data")
		dev2 := blockPartition("nvme0n1p2", "log")
		d.AddDevice(dev1)
		d.AddDevice(dev2)

		s := d.Slice(isBlock)
		ch, cancel := subscribeSlice(s)
		defer cancel()

		Eventually(ch).Should(Receive(ConsistOf(dev1, dev2)))
	})

	It("emits an empty slice from Init when no devices match the filter", func() {
		// Only a net device pre-populated — isBlock filter should exclude it.
		net := udev.NewFakeDevice("eth0").WithSubsystem(udev.NetSubsystem)
		d.AddDevice(net)

		s := d.Slice(isBlock)
		// Subscribe after Slice: goroutine replays current (empty) state.
		ch, cancel := subscribeSlice(s)
		defer cancel()

		Eventually(ch).Should(Receive(BeEmpty()))
	})

	It("emits an updated slice when a matching device is Added", func() {
		s := d.Slice(isBlock)
		ch, cancel := subscribeSlice(s)
		defer cancel()

		Eventually(ch).Should(Receive()) // consume Init snapshot

		dev := blockPartition("nvme0n1p1", "data")
		d.Emit(udev.Added{Device: dev})

		Eventually(ch).Should(Receive(ConsistOf(dev)))
	})

	It("does not emit when an Added device does not match the filter", func() {
		s := d.Slice(isBlock)
		ch, cancel := subscribeSlice(s)
		defer cancel()

		Eventually(ch).Should(Receive(BeEmpty())) // replayed empty Init

		// Add a non-matching (net) device — slice must not emit.
		net := udev.NewFakeDevice("eth0").WithSubsystem(udev.NetSubsystem)
		d.Emit(udev.Added{Device: net})

		// Add a matching device as a sequential fence: once its snapshot
		// arrives we know the net event was already processed and yielded
		// no emission.
		fence := blockPartition("nvme-fence", "fence")
		d.Emit(udev.Added{Device: fence})
		Eventually(ch).Should(Receive(ConsistOf(fence)))

		// No additional snapshot between the net event and the fence.
		Expect(ch).NotTo(Receive())
	})

	It("emits an updated slice when a tracked device is Removed", func() {
		dev1 := blockPartition("nvme0n1p1", "data")
		dev2 := blockPartition("nvme0n1p2", "log")
		d.AddDevice(dev1)
		d.AddDevice(dev2)

		s := d.Slice(isBlock)
		ch, cancel := subscribeSlice(s)
		defer cancel()

		Eventually(ch).Should(Receive(ConsistOf(dev1, dev2)))

		d.Emit(udev.Removed{Device: dev1})
		Eventually(ch).Should(Receive(ConsistOf(dev2)))
	})

	It("does not emit when a Removed device was not in the tracked set", func() {
		dev := blockPartition("nvme0n1p1", "data")
		d.AddDevice(dev)

		s := d.Slice(isBlock)
		ch, cancel := subscribeSlice(s)
		defer cancel()

		Eventually(ch).Should(Receive(ConsistOf(dev))) // replayed Init with dev

		// Remove a device that was never tracked (wrong subsystem).
		net := udev.NewFakeDevice("eth0").WithSubsystem(udev.NetSubsystem)
		d.Emit(udev.Removed{Device: net})

		// Fence: add a second matching device. When its snapshot arrives we
		// know the Removed(net) event was already processed with no emission.
		fence := blockPartition("nvme-fence", "fence")
		d.Emit(udev.Added{Device: fence})
		Eventually(ch).Should(Receive(ConsistOf(dev, fence)))

		// Exactly one snapshot: the fence addition, not the untracked removal.
		Expect(ch).NotTo(Receive())
	})

	It("reflects a sequence of Add then Remove correctly", func() {
		s := d.Slice(isBlock)
		ch, cancel := subscribeSlice(s)
		defer cancel()

		Eventually(ch).Should(Receive()) // consume empty Init

		dev := blockPartition("nvme0n1p1", "data")
		d.Emit(udev.Added{Device: dev})
		Eventually(ch).Should(Receive(ConsistOf(dev)))

		d.Emit(udev.Removed{Device: dev})
		Eventually(ch).Should(Receive(BeEmpty()))
	})

	It("delivers to multiple slice subscribers independently", func() {
		dev := blockPartition("nvme0n1p1", "data")

		s := d.Slice(isBlock)
		// Both subscribers get a replayed empty snapshot on Subscribe.
		ch1, c1 := subscribeSlice(s)
		ch2, c2 := subscribeSlice(s)
		defer c1()
		defer c2()

		Eventually(ch1).Should(Receive(BeEmpty()))
		Eventually(ch2).Should(Receive(BeEmpty()))

		d.Emit(udev.Added{Device: dev})
		Eventually(ch1).Should(Receive(ConsistOf(dev)))
		Eventually(ch2).Should(Receive(ConsistOf(dev)))
	})
})
