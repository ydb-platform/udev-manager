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

func (p *batchPartitionPool) add(dev udev.Device) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parts[dev.Id()] = dev
}

func (p *batchPartitionPool) remove(id udev.Id) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.parts, id)
}

func (p *batchPartitionPool) allocate(ctx context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	p.mu.RLock()
	snapshot := make([]udev.Device, 0, len(p.parts))
	for _, dev := range p.parts {
		snapshot = append(snapshot, dev)
	}
	p.mu.RUnlock()

	responses := make([]*pluginapi.ContainerAllocateResponse, 0, len(snapshot))
	for _, dev := range snapshot {
		partlabel := dev.Properties()[udev.PropertyPartName]
		responses = append(responses, allocatePartitionDevice(dev, p.domain, partlabel))
	}
	return mergeResponses(responses...), nil
}

// batchPartitionSeat is a single allocatable slot in a batch resource.
// Multiple seats share the same pool, allowing count concurrent allocations.
type batchPartitionSeat struct {
	id   Id
	pool *batchPartitionPool
}

func (s *batchPartitionSeat) Id() Id { return s.id }

func (s *batchPartitionSeat) Health() Health { return s.pool.health() }

func (s *batchPartitionSeat) TopologyHints() *pluginapi.TopologyInfo { return nil }

func (s *batchPartitionSeat) Allocate(ctx context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	return s.pool.allocate(ctx)
}

// matchBatchPartitionDevice checks if a device is a partition matching the given regexp.
// Returns the device's udev.Id and true if it matches, or zero value and false otherwise.
func matchBatchPartitionDevice(dev udev.Device, matcher *regexp.Regexp) (udev.Id, bool) {
	if dev.Subsystem() != udev.BlockSubsystem {
		return "", false
	}
	if dev.DevType() != udev.DeviceTypePart {
		return "", false
	}
	partlabel, found := dev.Properties()[udev.PropertyPartName]
	if !found {
		return "", false
	}
	if !matcher.MatchString(partlabel) {
		return "", false
	}
	return dev.Id(), true
}

// NewBatchPartitionScatter creates a batch partition resource that aggregates all partitions
// matching the given regexp into a single allocatable Kubernetes resource.
// count controls how many pods can simultaneously hold the resource (each gets all partitions).
func NewBatchPartitionScatter(
	d udev.Discovery,
	registry *Registry,
	domain string,
	name string,
	matcher *regexp.Regexp,
	count int,
) mux.CancelFunc {
	pool := &batchPartitionPool{
		parts:  make(map[udev.Id]udev.Device),
		domain: domain,
	}

	res := &resource{
		resourceTemplate: ResourceTemplate{
			Domain: domain,
			Prefix: "batch-" + name,
		},
		instances: make(map[Id]Instance, count),
		healthCh:  make(chan HealthEvent, 1),
	}

	seats := make([]Instance, count)
	for i := 0; i < count; i++ {
		seat := &batchPartitionSeat{
			id:   Id(fmt.Sprintf("%d", i)),
			pool: pool,
		}
		res.instances[seat.id] = seat
		seats[i] = seat
	}

	if err := registry.Add(res); err != nil {
		klog.Errorf("failed to add batch partition resource %s: %v", res.Name(), err)
		return func() {}
	}

	ch := make(chan udev.Event)
	go runBatchPartitionScatter(ch, pool, matcher, res, seats)

	return d.Subscribe(mux.SinkFromChan(ch))
}

func runBatchPartitionScatter(
	evCh <-chan udev.Event,
	pool *batchPartitionPool,
	matcher *regexp.Regexp,
	res *resource,
	seats []Instance,
) {
	for ev := range evCh {
		switch ev := ev.(type) {
		case udev.Init:
			for _, dev := range ev.Devices {
				if id, ok := matchBatchPartitionDevice(dev, matcher); ok {
					pool.add(dev)
					klog.V(5).Infof("batch %s: init matched partition %s", res.Name(), id)
				}
			}
			if !pool.empty() {
				if err := res.Submit(HealthEvent{Instances: seats}); err != nil {
					klog.Errorf("batch %s: failed to submit health event: %v", res.Name(), err)
				}
			}

		case udev.Added:
			id, ok := matchBatchPartitionDevice(ev.Device, matcher)
			if !ok {
				continue
			}
			wasEmpty := pool.empty()
			pool.add(ev.Device)
			klog.V(5).Infof("batch %s: added partition %s", res.Name(), id)
			if wasEmpty {
				if err := res.Submit(HealthEvent{Instances: seats}); err != nil {
					klog.Errorf("batch %s: failed to submit health event: %v", res.Name(), err)
				}
			}

		case udev.Removed:
			id, ok := matchBatchPartitionDevice(ev.Device, matcher)
			if !ok {
				continue
			}
			pool.remove(id)
			klog.V(5).Infof("batch %s: removed partition %s", res.Name(), id)
			if pool.empty() {
				if err := res.Submit(HealthEvent{Instances: seats}); err != nil {
					klog.Errorf("batch %s: failed to submit health event: %v", res.Name(), err)
				}
			}
		}
	}
}
