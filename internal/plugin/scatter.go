package plugin

import (
	"k8s.io/klog/v2"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"
)

type Scatter[T Instance] struct {
	mapper         FromDevice[T]
	registry       *Registry
	resourceDomain string
	resourcePrefix string
	routes         map[Id]Resource
}

func NewScatter[T Instance](d udev.Discovery, registry *Registry, resourceDomain, resourcePrefix string, mapper FromDevice[T]) mux.CancelFunc {
	scatter := &Scatter[T]{
		mapper:         mapper,
		registry:       registry,
		resourceDomain: resourceDomain,
		resourcePrefix: resourcePrefix,
		routes:         make(map[Id]Resource),
	}
	ch := make(chan udev.Event)

	go scatter.run(ch)

	return d.Subscribe(mux.SinkFromChan(ch))
}

func (s *Scatter[T]) add(id Id, resource Resource) {
	err := s.registry.Add(resource)
	if err != nil {
		klog.Errorf("failed to add resource %s: %v", resource.Name, err)
		return
	}
	s.routes[id] = resource
}

func (s *Scatter[T]) run(evCh <-chan udev.Event) {
	for ev := range evCh {
		switch ev := ev.(type) {
		case udev.Init:
			for _, dev := range ev.Devices {
				maybeInstance := s.mapper(dev)
				if maybeInstance == nil {
					continue
				}
				klog.V(5).Info("Init: Matched device: %s", dev.Debug())
				instance := *maybeInstance
				id := instance.Id()
				if _, ok := s.routes[id]; ok {
					continue
				}
				resource := &singletonResource{
					resourcePrefix: s.resourceDomain + "/" + s.resourcePrefix,
					instance:       instance,
					healthCh:       make(chan HealthEvent),
				}
				s.add(id, resource)
			}
		case udev.Added:
			maybeInstance := s.mapper(ev.Device)
			if maybeInstance == nil {
				continue
			}
			klog.V(5).Info("Added: Matched device: %s", ev.Device.Debug())
			instance := *maybeInstance
			id := instance.Id()
			if resource, ok := s.routes[id]; ok {
				resource.Submit(HealthEvent{instance, Healthy{}})
			} else {
				resource := &singletonResource{
					resourcePrefix: s.resourcePrefix,
					instance:       instance,
					healthCh:       make(chan HealthEvent),
				}
				s.add(id, resource)
			}
		case udev.Removed:
			maybeInstance := s.mapper(ev.Device)
			if maybeInstance == nil {
				continue
			}
			klog.V(5).Info("Removed: Matched device: %s", ev.Device.Debug())
			instance := *maybeInstance
			id := instance.Id()
			if resource, ok := s.routes[id]; ok {
				resource.Submit(HealthEvent{instance, Unhealthy{}})
			}
		}
	}
}
