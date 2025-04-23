package plugin

import (
	"k8s.io/klog/v2"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"
)

type Scatter[T Instance] struct {
	templater FromDevice[*ResourceTemplate]
	mapper    FromDevice[[]T]
	registry  *Registry
	routes    map[ResourceTemplate]Resource
}

func NewScatter[T Instance](
	d udev.Discovery,
	registry *Registry,
	templater FromDevice[*ResourceTemplate],
	mapper FromDevice[[]T],
) mux.CancelFunc {
	scatter := &Scatter[T]{
		templater: templater,
		mapper:    mapper,
		registry:  registry,
		routes:    make(map[ResourceTemplate]Resource),
	}
	ch := make(chan udev.Event)

	go scatter.run(ch)

	return d.Subscribe(mux.SinkFromChan(ch))
}

func (s *Scatter[T]) added(dev udev.Device) {
	if dev == nil {
		klog.Errorf("device is nil")
		return
	}

	template, err := s.templater(dev)
	if err != nil {
		klog.Errorf("failed to create resource template for device %q, caused by %q", dev.Debug(), err.Error())
		return
	}

	if template == nil {
		klog.V(5).Info("unmatched device: %q, template is nil", dev.Debug())
		return
	}

	instances, err := s.mapper(dev)
	if err != nil {
		klog.Errorf("failed to map device %q to instances, caused by %q", dev.Debug(), err.Error())
		return
	}

	klog.V(5).Info("Init: Matched device: %q", dev.Debug())

	if res, ok := s.routes[*template]; ok {
		klog.V(5).Info("Init: Matched resource: %s", res.Name())
		res.Submit(HealthEvent{
			Instances: unpack(instances...),
			Health:    Healthy{},
		})
		return
	}

	res := &resource{
		resourceTemplate: *template,
		instances:        make(map[Id]Instance),
		healthCh:        make(chan HealthEvent),
	}

	for _, instance := range instances {
		res.instances[instance.Id()] = instance
	}

	err = s.registry.Add(res)
	if err != nil {
		klog.Errorf("failed to add resource %s: %v", res.Name(), err)
		return
	}
	s.routes[*template] = res
}

func (s *Scatter[T]) removed(dev udev.Device) {
	if dev == nil {
		klog.Errorf("device is nil")
		return
	}

	template, err := s.templater(dev)
	if err != nil {
		klog.Errorf("failed to create resource template for 'Removed' event for device %q, caused by %q", dev.Debug(), err.Error())
		return
	}

	if template == nil {
		klog.V(5).Info("unmatched device: %q, template is nil", dev.Debug())
		return
	}

	instances, err := s.mapper(dev)
	if err != nil {
		klog.Errorf("failed to map device %q to instances for 'Removed', caused by %q", dev.Debug(), err.Error())
		return
	}

	klog.V(5).Info("Removed: Matched device: %q", dev.Debug())

	if res, ok := s.routes[*template]; ok {
		klog.V(5).Info("Removed: Matched resource: %s", res.Name())
		res.Submit(HealthEvent{
			Instances: unpack(instances...),
			Health:    Unhealthy{},
		})
	} else {
		klog.Errorf("failed to find resource for 'Removed' event for device %q", dev.Debug())
	}
}

func unpack[T Instance](instances ...T) []Instance {
	result := make([]Instance, len(instances))
	for i, instance := range instances {
		result[i] = instance
	}
	return result
}

func (s *Scatter[T]) run(evCh <-chan udev.Event) {
	for ev := range evCh {
		switch ev := ev.(type) {
		case udev.Init:
			for _, dev := range ev.Devices {
				s.added(dev)
			}
		case udev.Added:
			s.added(ev.Device)
		case udev.Removed:
			s.removed(ev.Device)
		}
	}
}
