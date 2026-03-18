package udev

import (
	"fmt"
	"sync"

	"github.com/ydb-platform/udev-manager/internal/mux"
)

// FakeDevice is an in-memory implementation of [Device] for use in tests.
// Construct one with [NewFakeDevice] and configure it with the With* builder
// methods.
type FakeDevice struct {
	id         Id
	parent     Device
	subsystem  string
	devType    string
	devNode    string
	devLinks   []string
	properties map[string]string
	sysattrs   map[string]string
	tags       []string
	numaNode   int
}

// NewFakeDevice returns a FakeDevice with the given ID. All fields default to
// their zero values except NumaNode, which defaults to -1 (no NUMA info).
func NewFakeDevice(id Id) *FakeDevice {
	return &FakeDevice{
		id:         id,
		properties: make(map[string]string),
		sysattrs:   make(map[string]string),
		numaNode:   -1,
	}
}

// Builder methods — each mutates the receiver and returns it for chaining.

// WithParent sets the parent device.
func (d *FakeDevice) WithParent(p Device) *FakeDevice { d.parent = p; return d }

// WithSubsystem sets the subsystem (e.g. "block", "net").
func (d *FakeDevice) WithSubsystem(s string) *FakeDevice { d.subsystem = s; return d }

// WithDevType sets the device type (e.g. "partition").
func (d *FakeDevice) WithDevType(t string) *FakeDevice { d.devType = t; return d }

// WithDevNode sets the device node path (e.g. "/dev/sda1").
func (d *FakeDevice) WithDevNode(n string) *FakeDevice { d.devNode = n; return d }

// WithNumaNode sets the NUMA node. Use -1 for "not available".
func (d *FakeDevice) WithNumaNode(n int) *FakeDevice { d.numaNode = n; return d }

// WithTags appends the given udev tags.
func (d *FakeDevice) WithTags(tags ...string) *FakeDevice { d.tags = append(d.tags, tags...); return d }

// WithProperty sets a udev property key/value pair.
func (d *FakeDevice) WithProperty(k, v string) *FakeDevice { d.properties[k] = v; return d }

// WithSysAttr sets a sysfs attribute key/value pair.
func (d *FakeDevice) WithSysAttr(k, v string) *FakeDevice { d.sysattrs[k] = v; return d }

// WithDevLinks appends the given device symlink paths.
func (d *FakeDevice) WithDevLinks(links ...string) *FakeDevice {
	d.devLinks = append(d.devLinks, links...)
	return d
}

// Device interface implementation.

// Id returns the device's unique identifier.
func (d *FakeDevice) Id() Id { return d.id }

// Parent returns the parent device, or nil if none was configured.
func (d *FakeDevice) Parent() Device { return d.parent }

// Subsystem returns the device subsystem.
func (d *FakeDevice) Subsystem() string { return d.subsystem }

// DevType returns the device type string.
func (d *FakeDevice) DevType() string { return d.devType }

// DevNode returns the device node path.
func (d *FakeDevice) DevNode() string { return d.devNode }

// DevLinks returns the device symlink paths.
func (d *FakeDevice) DevLinks() []string {
	if d.devLinks == nil {
		return nil
	}
	return d.devLinks
}

// Properties returns all udev properties as a map.
func (d *FakeDevice) Properties() map[string]string { return d.properties }

// Property returns the value of a single udev property.
func (d *FakeDevice) Property(key string) string { return d.properties[key] }

// PropertyLookup returns the property value for key on this device; if not
// found, it walks up to the parent device recursively.
func (d *FakeDevice) PropertyLookup(key string) string {
	if v := d.Property(key); v != "" {
		return v
	}
	if d.parent != nil {
		return d.parent.PropertyLookup(key)
	}
	return ""
}

// SystemAttributes returns all sysfs attributes as a map.
func (d *FakeDevice) SystemAttributes() map[string]string { return d.sysattrs }

// SystemAttribute returns the value of a single sysfs attribute.
func (d *FakeDevice) SystemAttribute(key string) string { return d.sysattrs[key] }

// SystemAttributeKeys returns the keys of all configured sysfs attributes.
func (d *FakeDevice) SystemAttributeKeys() []string {
	keys := make([]string, 0, len(d.sysattrs))
	for k := range d.sysattrs {
		keys = append(keys, k)
	}
	return keys
}

// SystemAttributeLookup returns the sysattr value for key; if not found,
// it walks up to the parent device recursively.
func (d *FakeDevice) SystemAttributeLookup(key string) string {
	if v := d.SystemAttribute(key); v != "" {
		return v
	}
	if d.parent != nil {
		return d.parent.SystemAttributeLookup(key)
	}
	return ""
}

// Tags returns the udev tags attached to this device.
func (d *FakeDevice) Tags() []string { return d.tags }

// NumaNode returns the NUMA node (-1 if not set).
func (d *FakeDevice) NumaNode() int { return d.numaNode }

// Debug returns a human-readable description of the device.
func (d *FakeDevice) Debug() string { return fmt.Sprintf("FakeDevice[%s]", d.id) }

// ---------------------------------------------------------------------------

// FakeDiscovery is an in-memory [Discovery] that test code drives by calling
// [FakeDiscovery.AddDevice] and [FakeDiscovery.Emit]. It implements the full
// Discovery interface and can be passed wherever a real udev Discovery is
// expected.
type FakeDiscovery struct {
	mu    sync.RWMutex
	state map[Id]Device
	m     *mux.Mux[Event]
}

// NewFakeDiscovery creates a FakeDiscovery with an empty device state.
func NewFakeDiscovery() *FakeDiscovery {
	return &FakeDiscovery{
		state: make(map[Id]Device),
		m:     mux.Make[Event](),
	}
}

// AddDevice inserts dev into the discovery's state without emitting an event.
// Call it before the first Subscribe so that the initial Init carries the
// device to all new subscribers.
func (f *FakeDiscovery) AddDevice(dev Device) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[dev.Id()] = dev
}

// Emit pushes ev to all current subscribers and updates the internal state:
// Added events add the device, Removed events delete it. The state update and
// event delivery are performed atomically under the lock to prevent races with
// concurrent Subscribe calls.
func (f *FakeDiscovery) Emit(ev Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch e := ev.(type) {
	case Added:
		f.state[e.Id()] = e.Device
	case Removed:
		delete(f.state, e.Id())
	}
	_ = f.m.Submit(ev)
}

// Subscribe delivers an [Init] event carrying the current device state to
// sink, then subscribes it to all subsequent events. The returned [mux.CancelFunc]
// unsubscribes sink.
//
// The Init delivery and mux subscription are performed atomically under a
// write lock to prevent races with [Emit]. The sink must not block in Submit
// (e.g., use a buffered channel) to avoid holding the lock.
func (f *FakeDiscovery) Subscribe(sink mux.Sink[Event]) mux.CancelFunc {
	// Hold the write lock across both the Init delivery and the mux
	// subscription so that no Emit can interleave between the two steps.
	f.mu.Lock()
	devices := make([]Device, 0, len(f.state))
	for _, dev := range f.state {
		devices = append(devices, dev)
	}
	_ = sink.Submit(Init{Devices: devices})
	cancel := f.m.Subscribe(sink)
	f.mu.Unlock()
	return cancel
}

// State returns the subset of the current device state that satisfies filter.
func (f *FakeDiscovery) State(filter mux.FilterFunc[Device]) map[Id]Device {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make(map[Id]Device)
	for k, v := range f.state {
		if filter(v) {
			result[k] = v
		}
	}
	return result
}

// DeviceById returns the device with the given ID, or nil if unknown.
func (f *FakeDiscovery) DeviceById(id Id) Device {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.state[id]
}

// Slice returns a [Slice] that tracks the filtered view of the device set.
func (f *FakeDiscovery) Slice(filter mux.FilterFunc[Device]) Slice {
	return makeSlice(f, filter)
}

// Close shuts down the FakeDiscovery and closes all subscriber sinks.
func (f *FakeDiscovery) Close() {
	f.m.Close()
}
