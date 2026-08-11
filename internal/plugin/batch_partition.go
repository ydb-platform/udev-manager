package plugin

import (
	"context"
	"fmt"
	"regexp"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"

	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// batchPartitionPool is the immutable backing for one reconciled generation of
// a batch resource. All seats in that generation share the pool, while a later
// generation gets a fresh pool and fresh seats. This lets Resource.Apply swap
// allocation and advertised state at the same linearization point.
type batchPartitionPool struct {
	parts  map[udev.Id]udev.Device
	labels map[udev.Id]string // mapped label per device (from capture group 1 or full PARTNAME)
	domain string
}

func (p *batchPartitionPool) health() Health {
	if len(p.parts) == 0 {
		return Unhealthy{}
	}
	return Healthy{}
}

func (p *batchPartitionPool) empty() bool {
	return len(p.parts) == 0
}

func (p *batchPartitionPool) allocate(ctx context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	type devLabel struct {
		dev   udev.Device
		label string
	}
	snapshot := make([]devLabel, 0, len(p.parts))
	for id, dev := range p.parts {
		snapshot = append(snapshot, devLabel{dev: dev, label: p.labels[id]})
	}

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
	snapshotCh := make(chan udev.Snapshot, 1)
	cancel := d.Subscribe(mux.SinkFromChan(snapshotCh))
	initial, ok := <-snapshotCh
	if !ok {
		cancel()
		return func() {}
	}

	instanceMap := batchPartitionInstances(initial, matcher, domain, count)

	res := newResource(ResourceTemplate{
		Domain: domain,
		Prefix: "batch-" + name,
	}, instanceMap)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		runBatchPartitionScatter(snapshotCh, matcher, domain, count, res)
	}()

	if err := registry.Add(res); err != nil {
		klog.Errorf("failed to add batch partition resource %s: %v", res.Name(), err)
		cancel()
		<-runDone
		res.Close()
		return func() {}
	}

	return func() {
		cancel()
		<-runDone
	}
}

func batchPartitionState(snapshot udev.Snapshot, matcher *regexp.Regexp) (map[udev.Id]udev.Device, map[udev.Id]string) {
	parts := make(map[udev.Id]udev.Device)
	labels := make(map[udev.Id]string)
	for _, dev := range snapshot.Devices {
		if id, label, ok := matchBatchPartitionDevice(dev, matcher); ok {
			parts[id] = dev
			labels[id] = label
		}
	}
	return parts, labels
}

// batchPartitionInstances builds a complete, immutable resource generation.
// Neither the pool nor its seats are reused by a later snapshot.
func batchPartitionInstances(
	snapshot udev.Snapshot,
	matcher *regexp.Regexp,
	domain string,
	count int,
) map[Id]Instance {
	parts, labels := batchPartitionState(snapshot, matcher)
	pool := &batchPartitionPool{
		parts:  parts,
		labels: labels,
		domain: domain,
	}
	instances := make(map[Id]Instance, count)
	for i := 0; i < count; i++ {
		seat := &batchPartitionSeat{
			id:   Id(fmt.Sprintf("%d", i)),
			pool: pool,
		}
		instances[seat.id] = seat
	}
	return instances
}

func runBatchPartitionScatter(
	snapshotCh <-chan udev.Snapshot,
	matcher *regexp.Regexp,
	domain string,
	count int,
	res *resource,
) {
	for snapshot := range snapshotCh {
		instances := batchPartitionInstances(snapshot, matcher, domain, count)
		if err := res.Apply(instances); err != nil {
			klog.Errorf("batch %s: failed to apply generation %d: %v", res.Name(), snapshot.Generation, err)
		}
	}
}
