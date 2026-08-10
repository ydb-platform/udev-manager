package plugin

import (
	"fmt"
	"reflect"
	"sort"

	"k8s.io/klog/v2"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"
)

// Scatter subscribes to authoritative udev snapshots and projects each one
// into complete desired Resource states. Each resource is committed once per
// snapshot, regardless of how many devices changed.
type Scatter[T Instance] struct {
	templater FromDevice[*ResourceTemplate]
	mapper    FromDevice[[]T]
	registry  *Registry
	routes    map[ResourceTemplate]Resource
	known     map[ResourceTemplate]map[Id]Instance
}

// NewScatter creates a Scatter that projects full discovery snapshots. The
// discovery subscription coalesces pending generations, so a slow projection
// consumes the newest authoritative state without accumulating delta events.
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
		known:     make(map[ResourceTemplate]map[Id]Instance),
	}
	ch := make(chan udev.Snapshot)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		scatter.run(ch)
	}()

	cancel := d.Subscribe(mux.SinkFromChan(ch))
	return func() {
		cancel()
		<-runDone
	}
}

// stage maps a complete device snapshot without mutating live resources. If
// any mapper returns an error, the whole projection is abandoned so a
// transient mapping failure cannot masquerade as removal of healthy devices.
func (s *Scatter[T]) stage(devices []udev.Device) (map[ResourceTemplate]map[Id]Instance, error) {
	desired := make(map[ResourceTemplate]map[Id]Instance)
	for _, dev := range devices {
		if dev == nil {
			return nil, fmt.Errorf("device is nil")
		}

		template, err := s.templater(dev)
		if err != nil {
			return nil, fmt.Errorf("create resource template for device %q: %w", dev.Debug(), err)
		}
		if template == nil {
			continue
		}

		instances, err := s.mapper(dev)
		if err != nil {
			return nil, fmt.Errorf("map device %q to instances: %w", dev.Debug(), err)
		}
		byID, ok := desired[*template]
		if !ok {
			byID = make(map[Id]Instance)
			desired[*template] = byID
		}
		for _, instance := range instances {
			if nilInstance(instance) {
				return nil, fmt.Errorf("device %q mapped to a nil instance", dev.Debug())
			}
			id := instance.Id()
			if _, duplicate := byID[id]; duplicate {
				return nil, fmt.Errorf("multiple devices map to resource %s/%s instance %q", template.Domain, template.Prefix, id)
			}
			byID[id] = instance
		}
	}
	return desired, nil
}

func nilInstance(instance Instance) bool {
	if instance == nil {
		return true
	}
	value := reflect.ValueOf(instance)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func baseInstance(instance Instance) Instance {
	for {
		override, ok := instance.(*healthOverride)
		if !ok {
			return instance
		}
		instance = override.Instance
	}
}

func instanceWithHealth(instance Instance, health Health) Instance {
	return &healthOverride{Instance: baseInstance(instance), health: health}
}

func (s *Scatter[T]) initializeKnown() {
	if s.known == nil {
		s.known = make(map[ResourceTemplate]map[Id]Instance)
	}
	for template, resource := range s.routes {
		if _, ok := s.known[template]; ok {
			continue
		}
		known := make(map[Id]Instance)
		for id, instance := range resource.Instances() {
			known[id] = baseInstance(instance)
		}
		s.known[template] = known
	}
}

// applySnapshot stages and atomically commits a full projection per resource.
// Historical instance IDs are retained as Unhealthy tombstones, preserving
// the previous device-plugin behavior for removed devices.
func (s *Scatter[T]) applySnapshot(snapshot udev.Snapshot) {
	present, err := s.stage(snapshot.Devices)
	if err != nil {
		klog.Errorf("scatter: failed to stage discovery generation %d: %v", snapshot.Generation, err)
		return
	}

	s.initializeKnown()
	templateSet := make(map[ResourceTemplate]struct{}, len(s.known)+len(present))
	for template := range s.known {
		templateSet[template] = struct{}{}
	}
	for template := range present {
		templateSet[template] = struct{}{}
	}
	templates := make([]ResourceTemplate, 0, len(templateSet))
	for template := range templateSet {
		templates = append(templates, template)
	}
	sort.Slice(templates, func(i, j int) bool {
		if templates[i].Domain == templates[j].Domain {
			return templates[i].Prefix < templates[j].Prefix
		}
		return templates[i].Domain < templates[j].Domain
	})

	for _, template := range templates {
		current, observed := present[template]
		known := s.known[template]
		if known == nil {
			known = make(map[Id]Instance)
			s.known[template] = known
		}
		for id, instance := range current {
			known[id] = baseInstance(instance)
		}

		desired := make(map[Id]Instance, len(known))
		for id, instance := range known {
			if currentInstance, ok := current[id]; ok {
				desired[id] = instanceWithHealth(currentInstance, Healthy{})
			} else {
				desired[id] = instanceWithHealth(instance, Unhealthy{})
			}
		}

		if resource, ok := s.routes[template]; ok {
			if err := resource.Apply(desired); err != nil {
				klog.Errorf("scatter: failed to apply generation %d to %s: %v", snapshot.Generation, resource.Name(), err)
			}
			continue
		}
		// A template is created only after it is observed in the current
		// snapshot. This also preserves resources whose mapper intentionally
		// produces an empty instance set.
		if !observed {
			continue
		}
		resource := newResource(template, desired)
		if s.registry == nil {
			klog.Errorf("scatter: cannot register resource %s without a registry", resource.Name())
			resource.Close()
			continue
		}
		if err := s.registry.Add(resource); err != nil {
			klog.Errorf("scatter: failed to add resource %s: %v", resource.Name(), err)
			resource.Close()
			continue
		}
		s.routes[template] = resource
	}
}

func (s *Scatter[T]) run(snapshotCh <-chan udev.Snapshot) {
	for snapshot := range snapshotCh {
		s.applySnapshot(snapshot)
	}
}
