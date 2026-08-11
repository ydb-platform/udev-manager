package plugin

import (
	"context"
	"errors"
	"sort"
	"sync"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/ydb-platform/udev-manager/internal/udev"
)

// Health is the sealed interface for device health states.
// The only concrete values are [Healthy] and [Unhealthy].
type Health interface {
	String() string
	sealed()
}

// Healthy indicates that a device instance is available for allocation.
type Healthy struct{}

func (Healthy) sealed() {}

func (Healthy) String() string {
	return "Healthy"
}

// Unhealthy indicates that a device instance is unavailable.
type Unhealthy struct{}

func (Unhealthy) sealed() {}

func (Unhealthy) String() string {
	return "Unhealthy"
}

// Id is the unique identifier of an [Instance] within a [Resource].
type Id string

// Instance represents a single allocatable device slot.
type Instance interface {
	Id() Id
	Health() Health
	TopologyHints() *pluginapi.TopologyInfo
	Allocate(context.Context) (*pluginapi.ContainerAllocateResponse, error)
}

// FromDevice is a function that maps a udev device to zero or more instances
// (or to a resource template). Returning nil, nil means the device does not
// match and should be ignored.
type FromDevice[T any] func(dev udev.Device) (T, error)

// Resource is a Kubernetes device-plugin resource backed by a set of
// [Instance] values. Apply atomically replaces its complete desired state;
// Watch notifies ListAndWatch handlers that they should pull the latest frozen
// kubelet-visible view from Devices.
type Resource interface {
	Name() string
	Instances() map[Id]Instance
	Devices() []*pluginapi.Device
	Apply(map[Id]Instance) error
	Watch(context.Context) <-chan struct{}
	Close()
}

// ResourceTemplate identifies a resource by its domain and name prefix,
// forming the full resource name "domain/prefix".
type ResourceTemplate struct {
	Domain string
	Prefix string
}

// healthOverride wraps an Instance, overriding its Health() return value.
// When the override health is Healthy, it delegates to the inner instance's
// own Health() so that instance-specific conditions (e.g. NIC operstate) are
// preserved. Only an Unhealthy override forces the health unconditionally.
type healthOverride struct {
	Instance
	health Health
}

func (h *healthOverride) Health() Health {
	if _, ok := h.health.(Healthy); ok {
		return h.Instance.Health()
	}
	return h.health
}

type resource struct {
	resourceTemplate ResourceTemplate
	mu               sync.RWMutex
	instances        map[Id]Instance
	devices          []*pluginapi.Device
	advertised       []advertisedDevice
	watchers         map[chan struct{}]struct{}
	closed           bool
	done             chan struct{}
	doneOnce         sync.Once
}

type advertisedDevice struct {
	id       string
	health   string
	numaNode []int64
}

var errResourceClosed = errors.New("resource is closed")

// newResource creates a resource with a fully initialized first state. A
// plugin can therefore begin ListAndWatch immediately after Registry.Add
// without observing a partially projected reconciliation.
func newResource(template ResourceTemplate, instances map[Id]Instance) *resource {
	instanceCopy := cloneInstances(instances)
	devices, advertised := freezeDevices(instanceCopy)
	return &resource{
		resourceTemplate: template,
		instances:        instanceCopy,
		devices:          devices,
		advertised:       advertised,
		watchers:         make(map[chan struct{}]struct{}),
		done:             make(chan struct{}),
	}
}

func (r *resource) Name() string {
	return r.resourceTemplate.Domain + "/" + r.resourceTemplate.Prefix
}

// Instances returns a snapshot copy of the current instance map.
// Safe to call concurrently with Apply.
func (r *resource) Instances() map[Id]Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot := make(map[Id]Instance, len(r.instances))
	for k, v := range r.instances {
		snapshot[k] = v
	}
	return snapshot
}

// Devices returns a deep copy of the current frozen kubelet-visible view.
// Health and topology are materialized during Apply, so a ListAndWatch response
// cannot mix fields from different reconciliation generations.
func (r *resource) Devices() []*pluginapi.Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneDevices(r.devices)
}

// Apply atomically replaces allocation backing and, when the serialized
// kubelet view changed, wakes every ListAndWatch subscriber exactly once at
// most. Allocation backing is replaced even for a visible no-op so metadata
// changes at a reused syspath take effect.
func (r *resource) Apply(instances map[Id]Instance) error {
	nextInstances := cloneInstances(instances)
	nextDevices, nextAdvertised := freezeDevices(nextInstances)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errResourceClosed
	}

	r.instances = nextInstances
	if advertisedDevicesEqual(r.advertised, nextAdvertised) {
		return nil
	}
	r.devices = nextDevices
	r.advertised = nextAdvertised
	r.notifyLocked()
	return nil
}

func cloneInstances(instances map[Id]Instance) map[Id]Instance {
	result := make(map[Id]Instance, len(instances))
	for id, instance := range instances {
		result[id] = instance
	}
	return result
}

func freezeDevices(instances map[Id]Instance) ([]*pluginapi.Device, []advertisedDevice) {
	ids := make([]string, 0, len(instances))
	byID := make(map[string]Instance, len(instances))
	for id, instance := range instances {
		key := string(id)
		ids = append(ids, key)
		byID[key] = instance
	}
	sort.Strings(ids)

	devices := make([]*pluginapi.Device, 0, len(ids))
	advertised := make([]advertisedDevice, 0, len(ids))
	for _, id := range ids {
		instance := byID[id]
		health := instance.Health().String()
		nodes := topologyNodes(instance.TopologyHints())
		devices = append(devices, &pluginapi.Device{
			ID:       id,
			Health:   health,
			Topology: topologyFromNodes(nodes),
		})
		advertised = append(advertised, advertisedDevice{
			id:       id,
			health:   health,
			numaNode: nodes,
		})
	}
	return devices, advertised
}

func topologyNodes(topology *pluginapi.TopologyInfo) []int64 {
	if topology == nil {
		return nil
	}
	nodes := make([]int64, 0, len(topology.Nodes))
	for _, node := range topology.Nodes {
		if node != nil {
			nodes = append(nodes, node.ID)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })
	return nodes
}

func topologyFromNodes(nodes []int64) *pluginapi.TopologyInfo {
	if len(nodes) == 0 {
		return nil
	}
	topology := &pluginapi.TopologyInfo{Nodes: make([]*pluginapi.NUMANode, len(nodes))}
	for i, id := range nodes {
		topology.Nodes[i] = &pluginapi.NUMANode{ID: id}
	}
	return topology
}

func advertisedDevicesEqual(a, b []advertisedDevice) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].id != b[i].id || a[i].health != b[i].health || len(a[i].numaNode) != len(b[i].numaNode) {
			return false
		}
		for j := range a[i].numaNode {
			if a[i].numaNode[j] != b[i].numaNode[j] {
				return false
			}
		}
	}
	return true
}

func cloneDevices(devices []*pluginapi.Device) []*pluginapi.Device {
	result := make([]*pluginapi.Device, len(devices))
	for i, device := range devices {
		if device == nil {
			continue
		}
		result[i] = &pluginapi.Device{
			ID:       device.ID,
			Health:   device.Health,
			Topology: topologyFromNodes(topologyNodes(device.Topology)),
		}
	}
	return result
}

// Watch registers a capacity-one dirty notification. The initial token and
// registration linearize under the same lock as Apply. Consumers must receive
// a token before calling Devices: an update either wins that read and is
// included, or happens afterward and leaves a token for the next read.
func (r *resource) Watch(ctx context.Context) <-chan struct{} {
	updates := make(chan struct{}, 1)
	r.mu.Lock()
	if r.closed {
		close(updates)
		r.mu.Unlock()
		return updates
	}
	r.watchers[updates] = struct{}{}
	updates <- struct{}{}
	r.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			r.removeWatcher(updates)
		case <-r.done:
		}
	}()
	return updates
}

func (r *resource) notifyLocked() {
	for updates := range r.watchers {
		select {
		case updates <- struct{}{}:
		default:
		}
	}
}

func (r *resource) removeWatcher(updates chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.watchers[updates]; !ok {
		return
	}
	delete(r.watchers, updates)
	close(updates)
}

// Close shuts down the resource and closes all subscriber channels.
func (r *resource) Close() {
	r.doneOnce.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.closed = true
		close(r.done)
		for updates := range r.watchers {
			delete(r.watchers, updates)
			close(updates)
		}
	})
}
