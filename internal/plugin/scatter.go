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
	templater      FromDevice[*ResourceTemplate]
	mapper         FromDevice[[]T]
	registry       *Registry
	routes         map[ResourceTemplate]Resource
	known          map[ResourceTemplate]map[Id]Instance
	deviceTemplate map[udev.Id]ResourceTemplate
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
		templater:      templater,
		mapper:         mapper,
		registry:       registry,
		routes:         make(map[ResourceTemplate]Resource),
		known:          make(map[ResourceTemplate]map[Id]Instance),
		deviceTemplate: make(map[udev.Id]ResourceTemplate),
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

type scatterStage struct {
	present        map[ResourceTemplate]map[Id]Instance
	failed         map[ResourceTemplate]struct{}
	deviceTemplate map[udev.Id]ResourceTemplate
	failedDevices  map[udev.Id]struct{}
	seenDevices    map[udev.Id]struct{}
	dependencies   map[ResourceTemplate]map[ResourceTemplate]struct{}
	errors         []error
	globalFailure  bool
}

func newScatterStage() *scatterStage {
	return &scatterStage{
		present:        make(map[ResourceTemplate]map[Id]Instance),
		failed:         make(map[ResourceTemplate]struct{}),
		deviceTemplate: make(map[udev.Id]ResourceTemplate),
		failedDevices:  make(map[udev.Id]struct{}),
		seenDevices:    make(map[udev.Id]struct{}),
		dependencies:   make(map[ResourceTemplate]map[ResourceTemplate]struct{}),
	}
}

func (stage *scatterStage) fail(err error, templates ...ResourceTemplate) {
	stage.errors = append(stage.errors, err)
	for _, template := range templates {
		stage.failed[template] = struct{}{}
	}
}

func (stage *scatterStage) depend(a, b ResourceTemplate) {
	if a == b {
		return
	}
	if stage.dependencies[a] == nil {
		stage.dependencies[a] = make(map[ResourceTemplate]struct{})
	}
	if stage.dependencies[b] == nil {
		stage.dependencies[b] = make(map[ResourceTemplate]struct{})
	}
	stage.dependencies[a][b] = struct{}{}
	stage.dependencies[b][a] = struct{}{}
}

// propagateFailures keeps a device move atomic across its old and new
// templates. If either side cannot be projected, neither side is committed.
func (stage *scatterStage) propagateFailures() {
	for changed := true; changed; {
		changed = false
		for failed := range stage.failed {
			for dependent := range stage.dependencies[failed] {
				if _, ok := stage.failed[dependent]; ok {
					continue
				}
				stage.failed[dependent] = struct{}{}
				changed = true
			}
		}
	}
}

// stage maps a complete device snapshot without mutating live resources.
// Failures are recorded per resource template: a bad device preserves that
// template's last committed state without preventing unrelated templates from
// advancing. Prior device routes identify the affected template even when the
// templater itself fails transiently.
func (s *Scatter[T]) stage(devices []udev.Device) *scatterStage {
	stage := newScatterStage()
	for _, dev := range devices {
		if dev == nil {
			stage.errors = append(stage.errors, fmt.Errorf("device is nil"))
			stage.globalFailure = true
			continue
		}
		deviceID := dev.Id()
		stage.seenDevices[deviceID] = struct{}{}
		previousTemplate, previouslyMapped := s.deviceTemplate[deviceID]

		template, err := s.templater(dev)
		if err != nil {
			wrapped := fmt.Errorf("create resource template for device %q: %w", dev.Debug(), err)
			stage.failedDevices[deviceID] = struct{}{}
			if template != nil && previouslyMapped {
				stage.fail(wrapped, *template, previousTemplate)
			} else if template != nil {
				stage.fail(wrapped, *template)
			} else if previouslyMapped {
				stage.fail(wrapped, previousTemplate)
			} else {
				stage.errors = append(stage.errors, wrapped)
				stage.globalFailure = true
			}
			continue
		}
		if template == nil {
			continue
		}
		if previouslyMapped {
			stage.depend(previousTemplate, *template)
		}

		instances, err := s.mapper(dev)
		if err != nil {
			wrapped := fmt.Errorf("map device %q to instances: %w", dev.Debug(), err)
			stage.failedDevices[deviceID] = struct{}{}
			if previouslyMapped {
				stage.fail(wrapped, *template, previousTemplate)
			} else {
				stage.fail(wrapped, *template)
			}
			continue
		}
		byID, ok := stage.present[*template]
		if !ok {
			byID = make(map[Id]Instance)
			stage.present[*template] = byID
		}
		valid := true
		for _, instance := range instances {
			if nilInstance(instance) {
				wrapped := fmt.Errorf("device %q mapped to a nil instance", dev.Debug())
				stage.failedDevices[deviceID] = struct{}{}
				if previouslyMapped {
					stage.fail(wrapped, *template, previousTemplate)
				} else {
					stage.fail(wrapped, *template)
				}
				valid = false
				break
			}
			id := instance.Id()
			if _, duplicate := byID[id]; duplicate {
				wrapped := fmt.Errorf("multiple devices map to resource %s/%s instance %q", template.Domain, template.Prefix, id)
				stage.failedDevices[deviceID] = struct{}{}
				if previouslyMapped {
					stage.fail(wrapped, *template, previousTemplate)
				} else {
					stage.fail(wrapped, *template)
				}
				valid = false
				break
			}
			byID[id] = instance
		}
		if valid {
			stage.deviceTemplate[deviceID] = *template
		}
	}
	stage.propagateFailures()
	return stage
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
	if s.deviceTemplate == nil {
		s.deviceTemplate = make(map[udev.Id]ResourceTemplate)
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

func (s *Scatter[T]) commitDeviceTemplates(stage *scatterStage) {
	next := make(map[udev.Id]ResourceTemplate, len(stage.deviceTemplate))

	// An absent device is forgotten only after its old template accepted the
	// authoritative absence. If that template failed, retain the route so a
	// later templater failure can still be isolated correctly.
	for deviceID, previousTemplate := range s.deviceTemplate {
		if _, seen := stage.seenDevices[deviceID]; seen {
			continue
		}
		if _, failed := stage.failed[previousTemplate]; failed {
			next[deviceID] = previousTemplate
		}
	}

	for deviceID := range stage.seenDevices {
		previousTemplate, previouslyMapped := s.deviceTemplate[deviceID]
		if _, failed := stage.failedDevices[deviceID]; failed {
			if previouslyMapped {
				next[deviceID] = previousTemplate
			}
			continue
		}

		currentTemplate, currentlyMapped := stage.deviceTemplate[deviceID]
		if currentlyMapped {
			if _, failed := stage.failed[currentTemplate]; !failed {
				next[deviceID] = currentTemplate
			} else if previouslyMapped {
				next[deviceID] = previousTemplate
			}
			continue
		}

		// A successful non-match removes the old route unless its resource was
		// held back transactionally because another device failed.
		if previouslyMapped {
			if _, failed := stage.failed[previousTemplate]; failed {
				next[deviceID] = previousTemplate
			}
		}
	}

	s.deviceTemplate = next
}

// applySnapshot stages and atomically commits a full projection per resource.
// Historical instance IDs are retained as Unhealthy tombstones, preserving
// the previous device-plugin behavior for removed devices.
func (s *Scatter[T]) applySnapshot(snapshot udev.Snapshot) {
	s.initializeKnown()
	stage := s.stage(snapshot.Devices)
	for _, err := range stage.errors {
		klog.Errorf("scatter: failed to stage part of discovery generation %d: %v", snapshot.Generation, err)
	}
	if stage.globalFailure {
		// Without a current or historical route, a failure cannot be scoped to
		// a resource safely. Preserve the whole last-good projection rather
		// than treating unknown devices as authoritative removals.
		return
	}

	templateSet := make(map[ResourceTemplate]struct{}, len(s.known)+len(stage.present))
	for template := range s.known {
		templateSet[template] = struct{}{}
	}
	for template := range stage.present {
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
		if _, failed := stage.failed[template]; failed {
			continue
		}
		current, observed := stage.present[template]
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

	s.commitDeviceTemplates(stage)
}

func (s *Scatter[T]) run(snapshotCh <-chan udev.Snapshot) {
	for snapshot := range snapshotCh {
		s.applySnapshot(snapshot)
	}
}
