package plugin

import (
	"context"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"
)

type Health interface {
	String() string
	sealed()
}

type Healthy struct{}

func (Healthy) sealed() {}

func (Healthy) String() string {
	return "Healthy"
}

type Unhealthy struct{}

func (Unhealthy) sealed() {}

func (Unhealthy) String() string {
	return "Unhealthy"
}

type Id string

type Instance interface {
	Id() Id
	Health() Health
	TopologyHints() *pluginapi.TopologyInfo
	Allocate(context.Context) (*pluginapi.ContainerAllocateResponse, error)
}

type healthInstance struct {
	Instance
	health Health
}

func (h *healthInstance) Id() Id {
	return h.Instance.Id()
}

func (h *healthInstance) Health() Health {
	if _, ok := h.health.(Healthy); ok {
		return h.Instance.Health()
	}
	return h.health
}

func (h *healthInstance) TopologyHints() *pluginapi.TopologyInfo {
	return h.Instance.TopologyHints()
}

func (h *healthInstance) Allocate(ctx context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	return h.Instance.Allocate(ctx)
}

type FromDevice[T any] func(dev udev.Device) (T, error)

type HealthEvent struct {
	Instances []Instance
	Health
}

type Resource interface {
	mux.Sink[HealthEvent]
	Name() string
	Instances() map[Id]Instance
	ListAndWatch(context.Context) <-chan []Instance
}

type ResourceTemplate struct {
	Domain string
	Prefix string
}

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
			r.instances[instance.Id()] = instance
		}
		instanceCh <- ev.Instances
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
