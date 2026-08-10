package udev

import (
	"github.com/ydb-platform/udev-manager/internal/mux"
)

// Id is the unique identifier for a device, typically its sysfs path.
type Id string

// Device represents a single udev device and exposes its attributes.
type Device interface {
	Id() Id
	Parent() Device
	Subsystem() string
	DevType() string
	DevNode() string
	DevLinks() []string
	Properties() map[string]string
	Property(string) string
	PropertyLookup(string) string
	SystemAttributes() map[string]string
	SystemAttributeKeys() []string
	SystemAttribute(string) string
	SystemAttributeLookup(string) string
	Tags() []string
	NumaNode() int

	Debug() string
}

// Event is a state mutation accepted by [FakeDiscovery.Emit]. Production
// discovery does not publish these edge-triggered values; subscribers receive
// authoritative [Snapshot] values instead. Its concrete types are [Added] and
// [Removed].
type Event interface {
	eventSealed()
}

// Added is emitted when a new device appears in the system.
type Added struct {
	Device
}

func (Added) eventSealed() {}

// Removed is emitted when a device is removed from the system.
type Removed struct {
	Device
}

func (Removed) eventSealed() {}

// Snapshot is an authoritative view produced by one successful udev
// enumeration. Generation increases on every successful pass, including
// passes whose device set is unchanged. Consumers may safely skip intermediate
// snapshots and process only the newest generation.
type Snapshot struct {
	Generation uint64
	Devices    []Device
}

// Slice is a filtered, live view of the device set. Subscribers receive a
// fresh []Device snapshot every time the matching set changes.
type Slice interface {
	mux.Source[[]Device]
}

// Discovery is the top-level interface for device enumeration and monitoring.
// Subscribe immediately delivers the latest authoritative [Snapshot], then
// subsequent snapshots produced by event-triggered or periodic reconciliation.
type Discovery interface {
	mux.Source[Snapshot]
	DeviceById(Id) Device
	State(mux.FilterFunc[Device]) map[Id]Device
	Slice(mux.FilterFunc[Device]) Slice
	Close()
}
