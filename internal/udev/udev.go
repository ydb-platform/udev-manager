package udev

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	libudev "github.com/jochenvg/go-udev"

	"k8s.io/klog/v2"

	"github.com/ydb-platform/udev-manager/internal/mux"
)

// Well-known udev subsystem names, device-type values, property keys,
// sysfs attribute names, and action strings used throughout the package.
const (
	BlockSubsystem = "block"
	NetSubsystem   = "net"

	DeviceTypeKey  = "DEVTYPE"
	DeviceTypePart = "partition"

	PropertyPartName    = "PARTNAME"
	PropertyModel       = "ID_MODEL"
	PropertyShortSerial = "ID_SERIAL_SHORT"

	PropertyInterface = "INTERFACE"

	SysAttrWWID   = "wwid"
	SysAttrModel  = "model"
	SysAttrSerial = "serial"

	SysAttrSpeed     = "speed"
	SysAttrOperstate = "operstate"

	ActionAdd     = "add"
	ActionRemove  = "remove"
	ActionOffline = "offline"
	ActionOnline  = "online"
)

type monitorRequest interface {
	requestSealed()
}

type stateRequest struct {
	filter mux.FilterFunc[Device]
}

func (r stateRequest) requestSealed() {}

type newSub struct {
	sink mux.Sink[Event]
}

func (n newSub) requestSealed() {}

type generic struct {
	udev Discovery

	dev        *libudev.Device
	parentOnce sync.Once
	parent     Device
}

func (g *generic) Id() Id {
	return Id(g.dev.Syspath())
}

func (g *generic) Parent() Device {
	g.parentOnce.Do(func() {
		if p := g.dev.Parent(); p != nil {
			g.parent = &generic{udev: g.udev, dev: p}
		}
	})
	return g.parent
}

func (g *generic) Subsystem() string {
	return g.dev.Subsystem()
}

func (g *generic) DevType() string {
	return g.dev.Devtype()
}

func (g *generic) DevNode() string {
	return g.dev.Devnode()
}

func (g *generic) DevLinks() []string {
	devlinks := g.dev.Devlinks()
	res := make([]string, 0, len(devlinks))
	for link := range devlinks {
		res = append(res, link)
	}
	return res
}

func (g *generic) Properties() map[string]string {
	return g.dev.Properties()
}

func (g *generic) Property(key string) string {
	return strings.TrimSpace(g.dev.PropertyValue(key))
}

func (g *generic) PropertyLookup(key string) string {
	value := g.Property(key)
	if value == "" {
		if p := g.Parent(); p != nil {
			return p.PropertyLookup(key)
		}
	}
	return value
}

func (g *generic) SystemAttributeKeys() []string {
	sysattrs := g.dev.Sysattrs()
	res := make([]string, 0, len(sysattrs))
	for attr := range sysattrs {
		res = append(res, attr)
	}
	return res
}

func (g *generic) SystemAttribute(key string) string {
	return strings.TrimSpace(g.dev.SysattrValue(key))
}

func (g *generic) SystemAttributes() map[string]string {
	res := make(map[string]string)
	for attr := range g.dev.Sysattrs() {
		res[attr] = g.SystemAttribute(attr)
	}
	return res
}

func (g *generic) SystemAttributeLookup(key string) string {
	value := g.SystemAttribute(key)
	if value == "" {
		if p := g.Parent(); p != nil {
			return p.SystemAttributeLookup(key)
		}
	}
	return value
}

func (g *generic) Tags() []string {
	tags := g.dev.Tags()
	res := make([]string, 0, len(tags))
	for tag := range tags {
		res = append(res, tag)
	}
	return res
}

func (g *generic) NumaNode() int {
	numaNodeStr := g.SystemAttributeLookup("numa_node")
	if numaNode, err := strconv.Atoi(numaNodeStr); err == nil {
		return numaNode
	}
	return -1
}

func (g *generic) Debug() string {
	return fmt.Sprintf("Device[ID=%s, Subsystem=%s, DevType=%s, DevNode=%s, NumaNode=%d, Links=%v, Tags=%v, Properties=%v, SysAttrs=%v]",
		g.Id(),
		g.Subsystem(),
		g.DevType(),
		g.DevNode(),
		g.NumaNode(),
		g.DevLinks(),
		g.Tags(),
		g.Properties(),
		g.SystemAttributes(),
	)
}

// subscribeReq is a request sent to the slice goroutine to register a new
// downstream sink. The goroutine is the sole owner of the slice state, so
// routing Subscribe through it lets us atomically snapshot and register
// without holding a mutex.
type subscribeReq struct {
	sink  mux.Sink[[]Device]
	reply chan mux.CancelFunc
}

type udevSlice struct {
	state      map[Id]Device
	filter     mux.FilterFunc[Device]
	mux        *mux.Mux[[]Device]
	stop       mux.CancelFunc
	subscribeC chan subscribeReq
	done       chan struct{} // closed when the slice goroutine exits
}

func (s *udevSlice) Close() {
	s.stop()
}

// Subscribe registers sink to receive every future device-set snapshot. It
// also immediately delivers the current snapshot to sink so the caller has a
// consistent starting view without any race against concurrent updates.
//
// The replay and the subsequent mux subscription happen inside the slice's
// goroutine, which is the sole owner of state, so there is no window where an
// update could be missed or delivered twice.
func (s *udevSlice) Subscribe(sink mux.Sink[[]Device]) mux.CancelFunc {
	replyCh := make(chan mux.CancelFunc)
	select {
	case s.subscribeC <- subscribeReq{sink: sink, reply: replyCh}:
		return <-replyCh
	case <-s.done:
		sink.Close()
		return func() {}
	}
}

type udevDiscovery struct {
	udev              libudev.Udev
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.RWMutex
	state             map[Id]Device
	requests          chan mux.AwaitReply[monitorRequest, any]
	mux               *mux.Mux[Event]
	wg                *sync.WaitGroup
	done              chan struct{} // closed when monitor exits
	reconcileInterval time.Duration
}

// DiscoveryOption configures a Discovery created by [NewDiscovery].
type DiscoveryOption func(*udevDiscovery)

// DefaultReconcileInterval bounds how long a lost udev event can keep the
// discovery state out of sync with sysfs.
const DefaultReconcileInterval = 60 * time.Second

// monitorReceiveBufferSize is the kernel socket buffer requested for the udev
// netlink monitor. The default (~208KB) overflows during boot-time coldplug
// storms, and libudev reports overflow indistinguishably from "no more data",
// silently dropping events.
const monitorReceiveBufferSize = 32 * 1024 * 1024

// WithReconcileInterval overrides how often the monitor re-enumerates devices
// to repair state drift caused by lost udev events.
func WithReconcileInterval(interval time.Duration) DiscoveryOption {
	return func(d *udevDiscovery) { d.reconcileInterval = interval }
}

// NewDiscovery creates a real udev-backed Discovery. It starts a monitor
// goroutine that opens the netlink socket, enumerates current devices, and
// then processes events. The socket is opened before enumeration so the
// kernel buffers events during the scan, eliminating the TOCTOU window.
//
// Because netlink events can still be lost (kernel socket overflow, monitor
// reconnects, slow consumers), the monitor additionally re-enumerates devices
// every reconcile interval and emits synthetic Added/Removed events for any
// drift it finds.
func NewDiscovery(wg *sync.WaitGroup, opts ...DiscoveryOption) (Discovery, error) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &udevDiscovery{
		state:             make(map[Id]Device),
		requests:          make(chan mux.AwaitReply[monitorRequest, any]),
		ctx:               ctx,
		cancel:            cancel,
		wg:                wg,
		done:              make(chan struct{}),
		reconcileInterval: DefaultReconcileInterval,
	}

	for _, opt := range opts {
		opt(d)
	}
	if d.reconcileInterval <= 0 {
		cancel()
		return nil, fmt.Errorf("reconcile interval must be positive, got %s", d.reconcileInterval)
	}

	// Start the mux only after option validation so an invalid configuration
	// does not leak its goroutine.
	d.mux = mux.Make[Event]()

	wg.Add(1)
	go d.monitor(wg)

	return d, nil
}

func (d *udevDiscovery) Close() {
	d.cancel()
	<-d.done
}

// State returns the current state of the devices as seen by the monitor
func (d *udevDiscovery) State(filter mux.FilterFunc[Device]) map[Id]Device {
	await := mux.NewAwaitReply[monitorRequest, any](stateRequest{filter: filter})
	select {
	case d.requests <- await:
		return await.Await().(map[Id]Device)
	case <-d.done:
		return nil
	}
}

func (d *udevDiscovery) DeviceById(id Id) Device {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.state[id]
}

func (d *udevDiscovery) Slice(filter mux.FilterFunc[Device]) Slice {
	return makeSlice(d, filter)
}

// makeSlice creates a Slice backed by any event Source. It subscribes to src
// (receiving an Init followed by Add/Remove events), applies filter, and
// publishes the current matching device set to downstream subscribers each
// time the set changes.
//
// Slice.Subscribe replays the current state to every new subscriber so callers
// always receive a consistent snapshot before any subsequent updates.
func makeSlice(src mux.Source[Event], filter mux.FilterFunc[Device]) Slice {
	slice := &udevSlice{
		state:      make(map[Id]Device),
		filter:     filter,
		mux:        mux.Make[[]Device](),
		subscribeC: make(chan subscribeReq),
		done:       make(chan struct{}),
	}

	evCh := make(chan Event)

	go func() {
		defer close(slice.done)
		defer slice.mux.Close()
		for {
			select {
			case ev, ok := <-evCh:
				if !ok {
					return
				}
				switch e := ev.(type) {
				case Init:
					for _, dev := range e.Devices {
						if filter(dev) {
							slice.state[dev.Id()] = dev
						}
					}
					if err := slice.mux.Submit(sliceSnapshot(slice.state)); err != nil {
						klog.Errorf("slice: failed to submit Init snapshot: %v", err)
					}
				case Added:
					if filter(e.Device) {
						slice.state[e.Id()] = e.Device
						if err := slice.mux.Submit(sliceSnapshot(slice.state)); err != nil {
							klog.Errorf("slice: failed to submit Added snapshot: %v", err)
						}
					}
				case Removed:
					if _, found := slice.state[e.Id()]; found {
						delete(slice.state, e.Id())
						if err := slice.mux.Submit(sliceSnapshot(slice.state)); err != nil {
							klog.Errorf("slice: failed to submit Removed snapshot: %v", err)
						}
					}
				}

			case req := <-slice.subscribeC:
				// Register with the mux first, then replay the current
				// snapshot. Because only this goroutine calls slice.mux.Submit,
				// no update can arrive between the two steps, so the subscriber
				// cannot miss a snapshot or receive one out of order.
				cancel := slice.mux.Subscribe(req.sink)
				// Replay the current snapshot via a goroutine to avoid
				// deadlocking when the sink wraps an unbuffered channel.
				// We send the reply first so the subscriber starts reading,
				// then wait for the replay to complete before processing
				// the next event (preserving snapshot ordering).
				snapshot := sliceSnapshot(slice.state)
				replayDone := make(chan struct{})
				go func() {
					defer close(replayDone)
					if err := req.sink.Submit(snapshot); err != nil {
						klog.Errorf("slice: failed to replay snapshot to new subscriber: %v", err)
					}
				}()
				req.reply <- cancel
				<-replayDone
			}
		}
	}()

	evSink := mux.SinkFromChan(evCh)
	slice.stop = src.Subscribe(evSink)

	return slice
}

// sliceSnapshot returns a stable copy of the device map as a slice.
func sliceSnapshot(state map[Id]Device) []Device {
	result := make([]Device, 0, len(state))
	for _, d := range state {
		result = append(result, d)
	}
	return result
}

// newMonitor creates a udev netlink monitor, requests a large kernel receive
// buffer, installs subsystem filters, and switches it to listening mode.
//
// The subsystem filters must cover every subsystem the plugin resource types
// consume (partitions -> block, network bandwidth / RDMA -> net). Extend the
// list when adding a resource type backed by another subsystem. Devices in
// other subsystems still show up via enumeration (State / reconcile), but
// their add/remove events are not delivered.
func (d *udevDiscovery) newMonitor() (<-chan *libudev.Device, <-chan error, error) {
	mon := d.udev.NewMonitorFromNetlink("udev")

	// Best effort: needs CAP_NET_ADMIN; without it we run with the default
	// buffer and rely on filters + reconciliation.
	if err := mon.SetReceiveBufferSize(monitorReceiveBufferSize); err != nil {
		klog.Warningf("Failed to set udev monitor receive buffer size: %v", err)
	}

	for _, subsystem := range []string{BlockSubsystem, NetSubsystem} {
		if err := mon.FilterAddMatchSubsystem(subsystem); err != nil {
			klog.Warningf("Failed to add udev monitor filter for subsystem %q: %v", subsystem, err)
		}
	}

	return mon.DeviceChan(d.ctx)
}

// newMonitorWithRetry opens a monitor, retrying transient setup failures while
// the discovery is alive. Making the delay context-aware keeps Close prompt
// even when udev is unavailable.
func (d *udevDiscovery) newMonitorWithRetry() (<-chan *libudev.Device, <-chan error, error) {
	for {
		if err := d.ctx.Err(); err != nil {
			return nil, nil, err
		}

		devChan, errChan, err := d.newMonitor()
		if err == nil {
			return devChan, errChan, nil
		}
		klog.Errorf("Failed to create device channel, retrying: %v", err)

		timer := time.NewTimer(time.Second)
		select {
		case <-timer.C:
		case <-d.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, nil, d.ctx.Err()
		}
	}
}

// applyEnumeration reconciles state with the set of devices found by an
// enumeration pass. Devices missing from state are inserted, devices no
// longer present are deleted. It returns the devices that were added and
// removed. Callers must hold no lock; state is the caller-owned map guarded
// by d.mu in the discovery case.
func applyEnumeration(state map[Id]Device, found map[Id]Device) (added, removed []Device) {
	for id, dev := range found {
		if _, ok := state[id]; !ok {
			state[id] = dev
			added = append(added, dev)
		}
	}
	for id, dev := range state {
		if _, ok := found[id]; !ok {
			delete(state, id)
			removed = append(removed, dev)
		}
	}
	return added, removed
}

// reconcile re-enumerates all devices and repairs any drift between sysfs and
// the tracked state, emitting synthetic Added/Removed events for the
// difference. This bounds the damage of lost udev events (kernel socket
// overflow, monitor reconnects): without it, a single lost event would leave
// the advertised resources wrong until the process restarts.
func (d *udevDiscovery) reconcile() {
	enum := d.udev.NewEnumerate()
	devs, err := enum.Devices()
	if err != nil {
		klog.Errorf("reconcile: failed to enumerate devices: %v", err)
		return
	}

	found := make(map[Id]Device, len(devs))
	for _, dev := range devs {
		if dev == nil {
			klog.Error("reconcile: udev device is nil!")
			continue
		}
		found[Id(dev.Syspath())] = &generic{
			udev: d,
			dev:  dev,
		}
	}

	d.mu.Lock()
	before := len(d.state)
	added, removed := applyEnumeration(d.state, found)
	d.mu.Unlock()

	if len(added) > 0 || len(removed) > 0 {
		if before == 0 && len(removed) == 0 {
			klog.V(4).Infof("reconcile: initial enumeration found %d devices", len(added))
		} else {
			klog.Warningf("reconcile: repaired state drift: %d missed additions, %d missed removals", len(added), len(removed))
		}
	}

	// Publish removals before additions. If a device is replaced at a new
	// syspath but maps to the same resource instance, this ensures the new
	// instance's Healthy update wins instead of the stale removal leaving it
	// Unhealthy.
	for _, dev := range removed {
		if err := d.mux.Submit(Removed{dev}); err != nil {
			klog.Errorf("reconcile: failed to submit Removed event: %v", err)
		}
	}
	for _, dev := range added {
		if err := d.mux.Submit(Added{dev}); err != nil {
			klog.Errorf("reconcile: failed to submit Added event: %v", err)
		}
	}
}

func (d *udevDiscovery) monitor(wg *sync.WaitGroup) {
	defer wg.Done()
	defer close(d.done)
	defer d.mux.Close()

	// Step 1: open the monitor socket so the kernel starts buffering events.
	devChan, errChan, err := d.newMonitorWithRetry()
	if err != nil {
		return
	}

	// Step 2: enumerate current devices while events buffer in devChan.
	// There are no subscribers yet, so the synthetic events go nowhere; this
	// pass only populates the initial state.
	d.reconcile()

	reconcileTicker := time.NewTicker(d.reconcileInterval)
	defer reconcileTicker.Stop()

	// Step 3: process buffered and future events.
	for {
		select {
		case <-d.ctx.Done():
			return
		case dev, ok := <-devChan:
			if !ok {
				klog.Warning("udev: monitor device channel closed, reconnecting")
				devChan, errChan, err = d.newMonitorWithRetry()
				if err != nil {
					return
				}
				d.reconcile()
				continue
			}
			klog.V(5).Infof("Received device event (%s): %s", dev.Action(), dev.Syspath())
			switch dev.Action() {
			case ActionAdd, ActionOnline:
				id := Id(dev.Syspath())
				dev := &generic{
					udev: d,
					dev:  dev,
				}
				d.mu.Lock()
				d.state[id] = dev
				d.mu.Unlock()
				if err := d.mux.Submit(Added{dev}); err != nil {
					klog.Errorf("udev: failed to submit Added event: %v", err)
				}
			case ActionRemove, ActionOffline:
				id := Id(dev.Syspath())
				d.mu.Lock()
				dev, ok := d.state[id]
				delete(d.state, id)
				d.mu.Unlock()
				if !ok {
					klog.V(5).Infof("udev: ignoring Remove for unknown device %s", id)
					continue
				}
				if err := d.mux.Submit(Removed{dev}); err != nil {
					klog.Errorf("udev: failed to submit Removed event: %v", err)
				}
			}
		case req := <-d.requests:
			switch r := req.Value().(type) {
			case stateRequest:
				d.mu.RLock()
				state := make(map[Id]Device)
				for k, v := range d.state {
					if r.filter(v) {
						state[k] = v
					}
				}
				d.mu.RUnlock()
				req.Reply(state)
			case newSub:
				d.mu.RLock()
				init := make([]Device, 0, len(d.state))
				for _, dev := range d.state {
					init = append(init, dev)
				}
				d.mu.RUnlock()
				err := r.sink.Submit(Init{init})
				if err != nil {
					klog.Errorf("Failed to submit init event: %v", err)
				}
				cancel := d.mux.Subscribe(r.sink)
				req.Reply(cancel)
			}
		case <-reconcileTicker.C:
			d.reconcile()
		case err, ok := <-errChan:
			if !ok {
				klog.Warning("udev: monitor error channel closed, reconnecting")
			} else {
				klog.Errorf("Error from udev monitor, reconnecting: %v", err)
			}
			devChan, errChan, err = d.newMonitorWithRetry()
			if err != nil {
				return
			}
			klog.Infof("Successfully reconnected to udev")
			// Events emitted while the monitor was down are gone; resync.
			d.reconcile()
		}
	}
}

func (d *udevDiscovery) Subscribe(sink mux.Sink[Event]) mux.CancelFunc {
	// here we're doing initialization in monitor goroutine
	// to be able to pass consistent Init event to the sink
	// before making fan out of udev events
	//
	// The sink is wrapped in an elastic buffer so that a slow consumer (e.g.
	// a scatter blocked on kubelet registration for seconds) cannot
	// back-pressure the fan-out: without it, one stalled subscriber causes
	// the monitor's mux.Submit to time out and drop the event for every
	// subscriber, permanently desynchronizing advertised resources.
	elastic := mux.ElasticSink(sink, klogLogger{})
	await := mux.NewAwaitReply[monitorRequest, any](newSub{elastic})
	select {
	case d.requests <- await:
		return await.Await().(mux.CancelFunc)
	case <-d.done:
		elastic.Close() // also closes the wrapped sink
		return func() {}
	}
}

// klogLogger adapts klog to the mux.Logger interface.
type klogLogger struct{}

func (klogLogger) Info(format string, args ...interface{}) {
	klog.Infof(format, args...)
}
