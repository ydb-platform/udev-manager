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
	broadcast        *mux.Mux[[]Instance]
	done             chan struct{}
	doneOnce         sync.Once
}

// newResource creates a resource. Submit updates are broadcast to all
// ListAndWatch subscribers via an internal mux.
func newResource(template ResourceTemplate, instances map[Id]Instance) *resource {
	return &resource{
		resourceTemplate: template,
		instances:        instances,
		broadcast:        mux.Make[[]Instance](),
		done:             make(chan struct{}),
	}
}

func (r *resource) Name() string {
	return r.resourceTemplate.Domain + "/" + r.resourceTemplate.Prefix
}

// Instances returns a snapshot copy of the current instance map.
// Safe to call concurrently with Submit.
func (r *resource) Instances() map[Id]Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot := make(map[Id]Instance, len(r.instances))
	for k, v := range r.instances {
		snapshot[k] = v
	}
	return snapshot
}

// Submit updates instance health state and broadcasts a snapshot to all
// active ListAndWatch subscribers.
func (r *resource) Submit(ev HealthEvent) error {
	r.mu.Lock()
	for _, instance := range ev.Instances {
		r.instances[instance.Id()] = &healthOverride{
			Instance: instance,
			health:   ev.Health,
		}
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	return r.broadcast.Submit(snapshot)
}

func (r *resource) snapshotLocked() []Instance {
	all := make([]Instance, 0, len(r.instances))
	for _, inst := range r.instances {
		all = append(all, inst)
	}
	return all
}

// ListAndWatch returns a channel that receives instance snapshots. Each call
// creates an independent subscriber that first receives the current state and
// then all subsequent updates. The subscription is cancelled when ctx is done.
// Safe to call multiple times (e.g. on kubelet reconnect).
func (r *resource) ListAndWatch(ctx context.Context) <-chan []Instance {
	ch := make(chan []Instance, 2)

	select {
	case <-r.done:
		close(ch)
		return ch
	default:
	}

	sink := mux.SinkFromChan(ch)

	// Hold RLock across subscribe + snapshot so that no Submit can
	// interleave between the two operations. This guarantees the
	// subscriber receives the replay snapshot before any future update.
	r.mu.RLock()
	cancel := r.broadcast.Subscribe(sink)
	snapshot := r.snapshotLocked()
	r.mu.RUnlock()

	// Replay current state. Non-blocking because ch has capacity 2.
	ch <- snapshot

	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-r.done:
		}
	}()

	return ch
}

// Close shuts down the resource and closes all subscriber channels.
func (r *resource) Close() {
	r.doneOnce.Do(func() {
		close(r.done)
		r.broadcast.Close()
	})
}
