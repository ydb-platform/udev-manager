package plugin

import (
	"context"

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
type healthOverride struct {
	Instance
	health Health
}

func (h *healthOverride) Health() Health { return h.health }

type resource struct {
	resourceTemplate ResourceTemplate
	instances        map[Id]Instance
	healthCh         chan HealthEvent
}

func (r *resource) Name() string {
	return r.resourceTemplate.Domain + "/" + r.resourceTemplate.Prefix
}

func (r *resource) Instances() map[Id]Instance {
	return r.instances
}

func (r *resource) Submit(ev HealthEvent) error {
	r.healthCh <- ev
	return nil
}

func (r *resource) run(instanceCh chan<- []Instance) {
	defer close(instanceCh)
	instances := make([]Instance, 0, len(r.instances))
	for _, instance := range r.instances {
		instances = append(instances, instance)
	}
	instanceCh <- instances
	for ev := range r.healthCh {
		for _, instance := range ev.Instances {
			r.instances[instance.Id()] = &healthOverride{
				Instance: instance,
				health:   ev.Health,
			}
		}
		all := make([]Instance, 0, len(r.instances))
		for _, inst := range r.instances {
			all = append(all, inst)
		}
		instanceCh <- all
	}
}

func (r *resource) ListAndWatch(ctx context.Context) <-chan []Instance {
	ch := make(chan []Instance, 1)

	go r.run(ch)

	return ch
}

func (r *resource) Close() {
	close(r.healthCh)
}
