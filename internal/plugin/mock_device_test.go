package plugin

import (
	"fmt"

	"github.com/ydb-platform/udev-manager/internal/udev"
)

// mockDevice implements udev.Device for use in tests.
type mockDevice struct {
	id         udev.Id
	subsystem  string
	devType    string
	devNode    string
	properties map[string]string
	sysattrs   map[string]string
	numaNode   int
	parent     udev.Device
}

func (m *mockDevice) Id() udev.Id         { return m.id }
func (m *mockDevice) Parent() udev.Device { return m.parent }
func (m *mockDevice) Subsystem() string   { return m.subsystem }
func (m *mockDevice) DevType() string     { return m.devType }
func (m *mockDevice) DevNode() string     { return m.devNode }
func (m *mockDevice) DevLinks() []string  { return nil }

func (m *mockDevice) Properties() map[string]string {
	if m.properties == nil {
		return map[string]string{}
	}
	return m.properties
}

func (m *mockDevice) Property(k string) string {
	return m.properties[k]
}

func (m *mockDevice) PropertyLookup(k string) string {
	if v, ok := m.properties[k]; ok {
		return v
	}
	if m.parent != nil {
		return m.parent.PropertyLookup(k)
	}
	return ""
}

func (m *mockDevice) SystemAttributes() map[string]string {
	if m.sysattrs == nil {
		return map[string]string{}
	}
	return m.sysattrs
}

func (m *mockDevice) SystemAttributeKeys() []string {
	keys := make([]string, 0, len(m.sysattrs))
	for k := range m.sysattrs {
		keys = append(keys, k)
	}
	return keys
}

func (m *mockDevice) SystemAttribute(k string) string {
	return m.sysattrs[k]
}

func (m *mockDevice) SystemAttributeLookup(k string) string {
	if v, ok := m.sysattrs[k]; ok {
		return v
	}
	if m.parent != nil {
		return m.parent.SystemAttributeLookup(k)
	}
	return ""
}

func (m *mockDevice) Tags() []string { return nil }
func (m *mockDevice) NumaNode() int  { return m.numaNode }
func (m *mockDevice) Debug() string  { return fmt.Sprintf("mockDevice{id:%s}", m.id) }

// partitionDevice constructs a mock block partition device with the given syspath id and partition label.
func partitionDevice(id, label string) *mockDevice {
	return &mockDevice{
		id:        udev.Id(id),
		subsystem: udev.BlockSubsystem,
		devType:   udev.DeviceTypePart,
		devNode:   "/dev/" + id,
		properties: map[string]string{
			udev.PropertyPartName: label,
		},
		sysattrs: map[string]string{},
		numaNode: -1,
	}
}

// netDevice constructs a mock network device with the given interface name, speed (Mbps), and operstate.
func netDevice(ifname, speed, operstate string) *mockDevice {
	return &mockDevice{
		id:        udev.Id(ifname),
		subsystem: udev.NetSubsystem,
		devType:   "net",
		properties: map[string]string{
			udev.PropertyInterface: ifname,
		},
		sysattrs: map[string]string{
			udev.SysAttrSpeed:     speed,
			udev.SysAttrOperstate: operstate,
		},
		numaNode: -1,
	}
}
