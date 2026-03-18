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

// Event is the sealed interface for udev events. The concrete types are
// [Init], [Added], and [Removed].
type Event interface {
	eventSealed()
}

// Init is the first event delivered to a new subscriber. It carries the
// full set of devices that existed at subscription time.
type Init struct {
	Devices []Device
}

func (Init) eventSealed() {}

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

// Slice is a filtered, live view of the device set. Subscribers receive a
// fresh []Device snapshot every time the matching set changes.
type Slice interface {
	mux.Source[[]Device]
}

// Discovery is the top-level interface for device enumeration and monitoring.
// Subscribe delivers an [Init] snapshot followed by [Added]/[Removed] events.
type Discovery interface {
	mux.Source[Event]
	DeviceById(Id) Device
	State(mux.FilterFunc[Device]) map[Id]Device
	Slice(mux.FilterFunc[Device]) Slice
	Close()
}
