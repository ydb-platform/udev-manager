package plugin

import (
	"context"
	"fmt"
	"regexp"
	"sync"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"

	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// batchPartitionPool holds the current set of partition devices matching a batch config entry.
// It is shared by all seats of the batch resource.
type batchPartitionPool struct {
	mu     sync.RWMutex
	parts  map[udev.Id]udev.Device
	labels map[udev.Id]string // mapped label per device (from capture group 1 or full PARTNAME)
	domain string
}

func (p *batchPartitionPool) health() Health {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.parts) == 0 {
		return Unhealthy{}
	}
	return Healthy{}
}

func (p *batchPartitionPool) empty() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.parts) == 0
}

func (p *batchPartitionPool) add(dev udev.Device, label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parts[dev.Id()] = dev
	p.labels[dev.Id()] = label
}

func (p *batchPartitionPool) remove(id udev.Id) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.parts, id)
	delete(p.labels, id)
}

func (p *batchPartitionPool) allocate(ctx context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	p.mu.RLock()
	type devLabel struct {
		dev   udev.Device
		label string
	}
	snapshot := make([]devLabel, 0, len(p.parts))
	for id, dev := range p.parts {
		snapshot = append(snapshot, devLabel{dev: dev, label: p.labels[id]})
	}
	p.mu.RUnlock()

	responses := make([]*pluginapi.ContainerAllocateResponse, 0, len(snapshot))
	for _, dl := range snapshot {
		responses = append(responses, allocatePartitionDevice(dl.dev, p.domain, dl.label))
	}
	return mergeResponses(responses...), nil
}

// batchPartitionSeat is a single allocatable slot in a batch resource.
// Multiple seats share the same pool, allowing count concurrent allocations.
type batchPartitionSeat struct {
	id   Id
	pool *batchPartitionPool
}

type batchPartitionHandler struct {
	registry *Registry
	pool     *batchPartitionPool
	matcher  *regexp.Regexp
	res      *resource
	seats    []Instance
}

func (s *batchPartitionSeat) Id() Id { return s.id }

func (s *batchPartitionSeat) Health() Health { return s.pool.health() }

func (s *batchPartitionSeat) TopologyHints() *pluginapi.TopologyInfo { return nil }

func (s *batchPartitionSeat) Allocate(ctx context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	return s.pool.allocate(ctx)
}

// matchBatchPartitionDevice checks if a device is a partition matching the given regexp.
// Returns the device's udev.Id, the mapped label (capture group 1 if present, otherwise
// the full PARTNAME), and true if it matches, or zero values and false otherwise.
func matchBatchPartitionDevice(dev udev.Device, matcher *regexp.Regexp) (udev.Id, string, bool) {
	if dev == nil {
		return "", "", false
	}
	if dev.Subsystem() != udev.BlockSubsystem {
		return "", "", false
	}
	if dev.DevType() != udev.DeviceTypePart {
		return "", "", false
	}
	partlabel, found := dev.Properties()[udev.PropertyPartName]
	if !found {
		return "", "", false
	}
	matches := matcher.FindStringSubmatch(partlabel)
	if len(matches) == 0 {
		return "", "", false
	}
	label := matches[0]
	if len(matches) > 1 {
		label = matches[1]
	}
	return dev.Id(), label, true
}

// NewBatchPartitionHandler creates a staged batch resource handler. The
// resource is not registered until InitComplete, after the complete initial
// udev snapshot has populated the pool.
func NewBatchPartitionHandler(
	registry *Registry,
	domain string,
	name string,
	matcher *regexp.Regexp,
	count int,
) DeviceHandler {
	pool := &batchPartitionPool{
		parts:  make(map[udev.Id]udev.Device),
		labels: make(map[udev.Id]string),
		domain: domain,
	}

	instanceMap := make(map[Id]Instance, count)
	seats := make([]Instance, count)
	for i := 0; i < count; i++ {
		seat := &batchPartitionSeat{
			id:   Id(fmt.Sprintf("%d", i)),
			pool: pool,
		}
		instanceMap[seat.id] = seat
		seats[i] = seat
	}

	res := newResource(ResourceTemplate{
		Domain: domain,
		Prefix: "batch-" + name,
	}, instanceMap)

	return &batchPartitionHandler{
		registry: registry,
		pool:     pool,
		matcher:  matcher,
		res:      res,
		seats:    seats,
	}
}

// NewBatchPartitionScatter is retained for standalone callers. startApp uses
// RunDeviceHandlers to share one discovery subscription across all handlers.
func NewBatchPartitionScatter(
	d udev.Discovery,
	registry *Registry,
	domain string,
	name string,
	matcher *regexp.Regexp,
	count int,
) mux.CancelFunc {
	handler := NewBatchPartitionHandler(registry, domain, name, matcher, count)
	return RunDeviceHandlers(d, nil, handler)
}

func (h *batchPartitionHandler) InitDevice(dev udev.Device) error {
	if id, label, ok := matchBatchPartitionDevice(dev, h.matcher); ok {
		h.pool.add(dev, label)
		klog.V(5).Infof("batch %s: init matched partition %s", h.res.Name(), id)
	}
	return nil
}

func (h *batchPartitionHandler) InitComplete() error {
	if !h.pool.empty() {
		if err := h.res.Submit(HealthEvent{Instances: h.seats, Health: Healthy{}}); err != nil {
			return fmt.Errorf("batch %s: failed to submit initial health: %w", h.res.Name(), err)
		}
	}
	if h.registry != nil {
		if err := h.registry.Add(h.res); err != nil {
			return fmt.Errorf("failed to add batch partition resource %s: %w", h.res.Name(), err)
		}
	}
	return nil
}

func (h *batchPartitionHandler) Added(dev udev.Device) error {
	id, label, ok := matchBatchPartitionDevice(dev, h.matcher)
	if !ok {
		return nil
	}
	wasEmpty := h.pool.empty()
	h.pool.add(dev, label)
	klog.V(5).Infof("batch %s: added partition %s", h.res.Name(), id)
	if wasEmpty {
		if err := h.res.Submit(HealthEvent{Instances: h.seats, Health: Healthy{}}); err != nil {
			return fmt.Errorf("batch %s: failed to submit healthy event: %w", h.res.Name(), err)
		}
	}
	return nil
}

func (h *batchPartitionHandler) Removed(dev udev.Device) error {
	id, _, ok := matchBatchPartitionDevice(dev, h.matcher)
	if !ok {
		return nil
	}
	h.pool.remove(id)
	klog.V(5).Infof("batch %s: removed partition %s", h.res.Name(), id)
	if h.pool.empty() {
		if err := h.res.Submit(HealthEvent{Instances: h.seats, Health: Unhealthy{}}); err != nil {
			return fmt.Errorf("batch %s: failed to submit unhealthy event: %w", h.res.Name(), err)
		}
	}
	return nil
}

func runBatchPartitionScatter(
	evCh <-chan udev.Event,
	pool *batchPartitionPool,
	matcher *regexp.Regexp,
	res *resource,
	seats []Instance,
) {
	handler := &batchPartitionHandler{
		pool:    pool,
		matcher: matcher,
		res:     res,
		seats:   seats,
	}
	for ev := range evCh {
		switch ev := ev.(type) {
		case udev.Init:
			for _, dev := range ev.Devices {
				_ = handler.InitDevice(dev)
			}
			if err := handler.InitComplete(); err != nil {
				klog.Errorf("batch %s: failed to finish initialization: %v", res.Name(), err)
			}

		case udev.Added:
			if err := handler.Added(ev.Device); err != nil {
				klog.Errorf("batch %s: failed to add device: %v", res.Name(), err)
			}

		case udev.Removed:
			if err := handler.Removed(ev.Device); err != nil {
				klog.Errorf("batch %s: failed to remove device: %v", res.Name(), err)
			}
		}
	}
}
