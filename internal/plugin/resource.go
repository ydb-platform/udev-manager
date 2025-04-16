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

type FromDevice[T Instance] func(dev udev.Device) *T

func (f FromDevice[T]) AsFilter() mux.FilterFunc[udev.Device] {
	return func(dev udev.Device) bool {
		return f(dev) != nil
	}
}

type HealthEvent struct {
	Instance
	Health
}

type Resource interface {
	mux.Sink[HealthEvent]
	Name() string
	Instances() map[Id]Instance
	ListAndWatch(context.Context) <-chan []Instance
}

type singletonResource struct {
	resourcePrefix string
	instance       Instance
	healthCh       chan HealthEvent
}

func (r *singletonResource) Name() string {
	return r.resourcePrefix + string(r.instance.Id())
}

func (r *singletonResource) Instances() map[Id]Instance {
	return map[Id]Instance{
		r.instance.Id(): r.instance,
	}
}

func (r *singletonResource) Submit(ev HealthEvent) error {
	r.healthCh <- ev
	return nil
}

func (r *singletonResource) run(instanceCh chan<- []Instance) {
	defer close(instanceCh)
	instanceCh <- []Instance{r.instance}
	for ev := range r.healthCh {
		r.instance = &healthInstance{
			Instance: r.instance,
			health:   ev.Health,
		}
		instanceCh <- []Instance{r.instance}
	}
}

func (r *singletonResource) ListAndWatch(ctx context.Context) <-chan []Instance {
	ch := make(chan []Instance, 1)

	go r.run(ch)

	return ch
}

func (r *singletonResource) Close() {
	close(r.healthCh)
}
