package plugin

import (
	"context"
	"sync"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/ydb-platform/udev-manager/internal/mux"
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

// HealthEvent carries a set of instances and their new health state to a
// [Resource].
type HealthEvent struct {
	Instances []Instance
	Health
}

// Resource is a Kubernetes device-plugin resource backed by a set of
// [Instance] values. It implements [mux.Sink] to receive health updates.
type Resource interface {
	mux.Sink[HealthEvent]
	Name() string
	Instances() map[Id]Instance
	ListAndWatch(context.Context) <-chan []Instance
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
	notify           chan struct{}
	instanceCh       chan []Instance
	done             chan struct{}
	doneOnce         sync.Once
}

// newResource creates a resource and starts its internal goroutine eagerly.
// This ensures Submit never blocks waiting for ListAndWatch to be called.
func newResource(template ResourceTemplate, instances map[Id]Instance) *resource {
	r := &resource{
		resourceTemplate: template,
		instances:        instances,
		notify:           make(chan struct{}, 1),
		instanceCh:       make(chan []Instance, 1),
		done:             make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *resource) Name() string {
	return r.resourceTemplate.Domain + "/" + r.resourceTemplate.Prefix
}

// Instances returns a snapshot copy of the current instance map.
// Safe to call concurrently with run().
func (r *resource) Instances() map[Id]Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot := make(map[Id]Instance, len(r.instances))
	for k, v := range r.instances {
		snapshot[k] = v
	}
	return snapshot
}

// Submit updates instance health state and notifies the run loop.
// Submit never blocks: if a notification is already pending, the update
// is coalesced — run() will pick up the latest state when it drains.
func (r *resource) Submit(ev HealthEvent) error {
	r.mu.Lock()
	for _, instance := range ev.Instances {
		r.instances[instance.Id()] = &healthOverride{
			Instance: instance,
			health:   ev.Health,
		}
	}
	r.mu.Unlock()

	select {
	case r.notify <- struct{}{}:
	case <-r.done:
	default:
	}
	return nil
}

func (r *resource) snapshot() []Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := make([]Instance, 0, len(r.instances))
	for _, inst := range r.instances {
		all = append(all, inst)
	}
	return all
}

// run is the single goroutine that publishes instance snapshots to
// instanceCh. It starts eagerly (via newResource) so that Submit
// never blocks waiting for ListAndWatch to be called.
func (r *resource) run() {
	defer close(r.instanceCh)
	select {
	case r.instanceCh <- r.snapshot():
	case <-r.done:
		return
	}
	for {
		select {
		case <-r.notify:
			select {
			case r.instanceCh <- r.snapshot():
			case <-r.done:
				return
			}
		case <-r.done:
			return
		}
	}
}

// ListAndWatch returns the channel fed by the resource's run loop.
// It must be called at most once per resource lifetime.
func (r *resource) ListAndWatch(_ context.Context) <-chan []Instance {
	return r.instanceCh
}

// Close shuts down the resource's run loop and closes the instance channel.
func (r *resource) Close() {
	r.doneOnce.Do(func() { close(r.done) })
}
