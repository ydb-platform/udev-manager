package udev

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// devMap builds an Id-keyed map from a set of devices, as diffDevices expects.
func devMap(devs ...Device) map[Id]Device {
	m := make(map[Id]Device, len(devs))
	for _, d := range devs {
		m[d.Id()] = d
	}
	return m
}

var _ = Describe("diffDevices", func() {
	It("returns nothing when both sets are empty", func() {
		added, removed := diffDevices(devMap(), devMap())
		Expect(added).To(BeEmpty())
		Expect(removed).To(BeEmpty())
	})

	It("returns nothing when the sets are identical", func() {
		a := NewFakeDevice("sysfs/nvme0n1p1")
		b := NewFakeDevice("sysfs/nvme0n1p2")
		added, removed := diffDevices(devMap(a, b), devMap(a, b))
		Expect(added).To(BeEmpty())
		Expect(removed).To(BeEmpty())
	})

	It("reports devices present in the enumeration but missing from state as added", func() {
		existing := NewFakeDevice("sysfs/nvme0n1p1")
		appeared := NewFakeDevice("sysfs/nvme0n1p2")
		added, removed := diffDevices(devMap(existing), devMap(existing, appeared))
		Expect(added).To(ConsistOf(appeared))
		Expect(removed).To(BeEmpty())
	})

	It("reports devices present in state but missing from the enumeration as removed", func() {
		existing := NewFakeDevice("sysfs/nvme0n1p1")
		vanished := NewFakeDevice("sysfs/nvme0n1p2")
		added, removed := diffDevices(devMap(existing, vanished), devMap(existing))
		Expect(added).To(BeEmpty())
		Expect(removed).To(ConsistOf(vanished))
	})

	It("reports simultaneous additions and removals", func() {
		kept := NewFakeDevice("sysfs/nvme0n1p1")
		vanished := NewFakeDevice("sysfs/nvme0n1p2")
		appeared := NewFakeDevice("sysfs/nvme0n1p3")
		added, removed := diffDevices(devMap(kept, vanished), devMap(kept, appeared))
		Expect(added).To(ConsistOf(appeared))
		Expect(removed).To(ConsistOf(vanished))
	})

	It("does not mutate its inputs", func() {
		existing := NewFakeDevice("sysfs/nvme0n1p1")
		appeared := NewFakeDevice("sysfs/nvme0n1p2")
		current := devMap(existing)
		enumerated := devMap(existing, appeared)
		diffDevices(current, enumerated)
		Expect(current).To(HaveLen(1))
		Expect(enumerated).To(HaveLen(2))
	})
})
