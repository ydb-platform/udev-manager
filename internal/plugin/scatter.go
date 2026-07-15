package plugin

import (
	"errors"
	"fmt"
	"sort"

	"k8s.io/klog/v2"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"
)

// Scatter subscribes to a udev [Discovery] and dynamically creates or updates
// [Resource] instances as matching devices are added or removed. Each unique
// ResourceTemplate produced by the templater gets its own Resource.
type Scatter[T Instance] struct {
	templater  FromDevice[*ResourceTemplate]
	mapper     FromDevice[[]T]
	registry   *Registry
	routes     map[ResourceTemplate]Resource
	registered map[ResourceTemplate]bool
}

// NewScatterHandler creates a handler that routes matching devices to
// resources via templater and mapper. Initial resources are staged until all
// configured handlers have processed the complete udev snapshot.
func NewScatterHandler[T Instance](
	registry *Registry,
	templater FromDevice[*ResourceTemplate],
	mapper FromDevice[[]T],
) DeviceHandler {
	return &Scatter[T]{
		templater:  templater,
		mapper:     mapper,
		registry:   registry,
		routes:     make(map[ResourceTemplate]Resource),
		registered: make(map[ResourceTemplate]bool),
	}
}

// NewScatter is retained for callers that need a single standalone scatter.
// startApp uses RunDeviceHandlers so every configured scatter shares one udev
// subscription and one initialization barrier.
func NewScatter[T Instance](
	d udev.Discovery,
	registry *Registry,
	templater FromDevice[*ResourceTemplate],
	mapper FromDevice[[]T],
) mux.CancelFunc {
	handler := NewScatterHandler(registry, templater, mapper)
	return RunDeviceHandlers(d, nil, handler)
}

func (s *Scatter[T]) InitDevice(dev udev.Device) error {
	return s.addedWithRegistration(dev, false)
}

func (s *Scatter[T]) InitComplete() error {
	if s.registered == nil {
		s.registered = make(map[ResourceTemplate]bool)
	}

	templates := make([]ResourceTemplate, 0, len(s.routes))
	for template := range s.routes {
		templates = append(templates, template)
	}
	sort.Slice(templates, func(i, j int) bool {
		return s.routes[templates[i]].Name() < s.routes[templates[j]].Name()
	})

	var registrationErrors []error
	for _, template := range templates {
		if s.registered[template] {
			continue
		}
		res := s.routes[template]
		if err := s.registry.Add(res); err != nil {
			registrationErrors = append(registrationErrors,
				fmt.Errorf("failed to register resource %s: %w", res.Name(), err))
			continue
		}
		s.registered[template] = true
	}
	return errors.Join(registrationErrors...)
}

func (s *Scatter[T]) Added(dev udev.Device) error {
	return s.added(dev)
}

func (s *Scatter[T]) Removed(dev udev.Device) error {
	return s.removed(dev)
}

func (s *Scatter[T]) added(dev udev.Device) error {
	return s.addedWithRegistration(dev, true)
}

func (s *Scatter[T]) addedWithRegistration(dev udev.Device, register bool) error {
	if dev == nil {
		return errors.New("device is nil")
	}
	template, err := s.templater(dev)
	if err != nil {
		return fmt.Errorf("failed to create resource template for device %q: %w", dev.Debug(), err)
	}

	if template == nil {
		if klog.V(5).Enabled() {
			klog.Infof("unmatched device: %q, template is nil", dev.Debug())
		}
		return nil
	}

	instances, err := s.mapper(dev)
	if err != nil {
		return fmt.Errorf("failed to map device %q to instances: %w", dev.Debug(), err)
	}

	if klog.V(5).Enabled() {
		klog.Infof("Matched device: %q", dev.Debug())
	}

	if res, ok := s.routes[*template]; ok {
		klog.V(5).Infof("Init: Matched resource: %s", res.Name())
		if err := res.Submit(HealthEvent{
			Instances: unpack(instances...),
			Health:    Healthy{},
		}); err != nil {
			return fmt.Errorf("failed to submit health event for %s: %w", res.Name(), err)
		}
		return nil
	}

	instanceMap := make(map[Id]Instance, len(instances))
	for _, instance := range instances {
		instanceMap[instance.Id()] = instance
	}
	res := newResource(*template, instanceMap)

	if register {
		if err := s.registry.Add(res); err != nil {
			res.Close()
			return fmt.Errorf("failed to add resource %s: %w", res.Name(), err)
		}
		if s.registered == nil {
			s.registered = make(map[ResourceTemplate]bool)
		}
		s.registered[*template] = true
	}
	s.routes[*template] = res
	return nil
}

func (s *Scatter[T]) removed(dev udev.Device) error {
	if dev == nil {
		return errors.New("device is nil")
	}
	template, err := s.templater(dev)
	if err != nil {
		return fmt.Errorf("failed to create resource template for removed device %q: %w", dev.Debug(), err)
	}

	if template == nil {
		if klog.V(5).Enabled() {
			klog.Infof("unmatched device: %q, template is nil", dev.Debug())
		}
		return nil
	}

	instances, err := s.mapper(dev)
	if err != nil {
		return fmt.Errorf("failed to map removed device %q to instances: %w", dev.Debug(), err)
	}

	if klog.V(5).Enabled() {
		klog.Infof("Removed: Matched device: %q", dev.Debug())
	}

	if res, ok := s.routes[*template]; ok {
		klog.V(5).Infof("Removed: Matched resource: %s", res.Name())
		if err := res.Submit(HealthEvent{
			Instances: unpack(instances...),
			Health:    Unhealthy{},
		}); err != nil {
			return fmt.Errorf("failed to submit health event for %s: %w", res.Name(), err)
		}
	} else {
		return fmt.Errorf("failed to find resource for removed device %q", dev.Debug())
	}
	return nil
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
				if err := s.InitDevice(dev); err != nil {
					klog.Errorf("failed to initialize device %q: %v", deviceID(dev), err)
				}
			}
			if err := s.InitComplete(); err != nil {
				klog.Errorf("failed to publish initial resources: %v", err)
			}
		case udev.Added:
			if err := s.Added(ev.Device); err != nil {
				klog.Errorf("failed to add device %q: %v", deviceID(ev.Device), err)
			}
		case udev.Removed:
			if err := s.Removed(ev.Device); err != nil {
				klog.Errorf("failed to remove device %q: %v", deviceID(ev.Device), err)
			}
		}
	}
}
