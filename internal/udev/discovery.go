package udev

import (
	"github.com/ydb-platform/udev-manager/internal/mux"
)

type Id string

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

type Event interface {
	eventSealed()
}

type Init struct {
	Devices []Device
}

func (Init) eventSealed() {}

type Added struct {
	Device
}

func (Added) eventSealed() {}

type Removed struct {
	Device
}

func (Removed) eventSealed() {}

type Slice interface {
	mux.Source[[]Device]
}

type Discovery interface {
	mux.Source[Event]
	DeviceById(Id) Device
	State(mux.FilterFunc[Device]) map[Id]Device
	Slice(mux.FilterFunc[Device]) Slice
	Close()
}
