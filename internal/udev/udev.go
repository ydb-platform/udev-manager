package udev

import (
	"context"
	"fmt"
	"reflect"
	"sort"
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
	sink mux.Sink[Snapshot]
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
	latest := mux.LatestSink(sink)
	replyCh := make(chan mux.CancelFunc)
	select {
	case s.subscribeC <- subscribeReq{sink: latest, reply: replyCh}:
		return <-replyCh
	case <-s.done:
		latest.Close()
		return func() {}
	}
}

type udevDiscovery struct {
	udev              libudev.Udev
	monitorUdev       libudev.Udev
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.RWMutex
	state             map[Id]Device
	offline           map[Id]uint64
	actionSequence    uint64
	requests          chan mux.AwaitReply[monitorRequest, any]
	mux               *mux.Mux[Snapshot]
	wg                *sync.WaitGroup
	done              chan struct{} // closed when the reconciliation controller exits
	eventsDone        chan struct{} // closed when the netlink pump exits
	monitorReady      chan struct{} // closed after the first monitor socket is listening
	reconcileC        chan struct{} // capacity-one dirty notification
	reconcileInterval time.Duration
	generation        uint64
}

// DiscoveryOption configures a Discovery created by [NewDiscovery].
type DiscoveryOption func(*udevDiscovery)

// DefaultReconcileInterval bounds how long a lost udev event can keep the
// discovery state out of sync with sysfs.
const DefaultReconcileInterval = 60 * time.Second

const reconcileRetryInterval = time.Second

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

// NewDiscovery creates a real udev-backed Discovery. Netlink notifications
// never mutate device membership directly; they trigger reconciliation, while
// offline/online lifecycle actions maintain an exclusion overlay for devices
// that remain enumerable while unavailable. A successful enumeration replaces
// the authoritative state and publishes one full Snapshot. The socket is
// opened before the initial enumeration so an event arriving during a scan
// schedules a trailing pass.
//
// Periodic reconciliation remains as a repair path for netlink overflow and
// monitor outages.
func NewDiscovery(wg *sync.WaitGroup, opts ...DiscoveryOption) (Discovery, error) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &udevDiscovery{
		state:             make(map[Id]Device),
		offline:           make(map[Id]uint64),
		requests:          make(chan mux.AwaitReply[monitorRequest, any]),
		ctx:               ctx,
		cancel:            cancel,
		wg:                wg,
		done:              make(chan struct{}),
		eventsDone:        make(chan struct{}),
		monitorReady:      make(chan struct{}),
		reconcileC:        make(chan struct{}, 1),
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
	d.mux = mux.Make[Snapshot]()

	wg.Add(2)
	go d.monitorEvents(wg)
	go d.monitor(wg)

	return d, nil
}

func (d *udevDiscovery) Close() {
	d.cancel()
	<-d.done
	<-d.eventsDone
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

// makeSlice creates a Slice backed by authoritative discovery snapshots. It
// replaces its filtered state on every generation and publishes when matching
// membership or any matching Device observation changes.
//
// Slice.Subscribe replays the current state to every new subscriber so callers
// always receive a consistent snapshot before any subsequent updates.
func makeSlice(src mux.Source[Snapshot], filter mux.FilterFunc[Device]) Slice {
	slice := &udevSlice{
		state:      make(map[Id]Device),
		filter:     filter,
		mux:        mux.Make[[]Device](),
		subscribeC: make(chan subscribeReq),
		done:       make(chan struct{}),
	}

	snapshotCh := make(chan Snapshot)
	ready := make(chan struct{})

	go func() {
		defer close(slice.done)
		defer slice.mux.Close()
		initialized := false
		for {
			select {
			case snapshot, ok := <-snapshotCh:
				if !ok {
					return
				}
				next := make(map[Id]Device)
				for _, dev := range snapshot.Devices {
					if filter(dev) {
						next[dev.Id()] = dev
					}
				}
				stateChanged := !initialized || !sameDeviceState(slice.state, next)
				initialized = true
				slice.state = next
				if !stateChanged {
					continue
				}
				if err := slice.mux.Submit(sliceSnapshot(slice.state)); err != nil {
					klog.Errorf("slice: failed to submit snapshot generation %d: %v", snapshot.Generation, err)
				}
				if initialized {
					select {
					case <-ready:
					default:
						close(ready)
					}
				}

			case req := <-slice.subscribeC:
				// Register with the mux first, then replay the current
				// snapshot. Because only this goroutine calls slice.mux.Submit,
				// no update can arrive between the two steps, so the subscriber
				// cannot miss a snapshot or receive one out of order.
				cancel := slice.mux.Subscribe(req.sink)
				snapshot := sliceSnapshot(slice.state)
				if err := req.sink.Submit(snapshot); err != nil {
					klog.Errorf("slice: failed to replay snapshot to new subscriber: %v", err)
				}
				req.reply <- cancel
			}
		}
	}()

	slice.stop = src.Subscribe(mux.SinkFromChan(snapshotCh))
	select {
	case <-ready:
	case <-slice.done:
	}

	return slice
}

// sameDeviceState compares both membership and device identity. Device values
// are immutable observations: replacing the value at a retained syspath can
// carry changed properties or sysattrs and must therefore reach existing Slice
// subscribers. Production enumeration creates fresh generic values, so a Slice
// intentionally refreshes on each successful pass; LatestSink bounds slow
// subscribers to the newest complete view.
func sameDeviceState(a, b map[Id]Device) bool {
	if len(a) != len(b) {
		return false
	}
	for id, current := range a {
		next, ok := b[id]
		if !ok || !sameDeviceIdentity(current, next) {
			return false
		}
	}
	return true
}

// sameDeviceIdentity avoids comparing interface values whose concrete type is
// not comparable. Such values cannot express stable identity, so conservatively
// treat them as replacements and publish the new observation.
func sameDeviceIdentity(a, b Device) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	typ := reflect.TypeOf(a)
	if typ != reflect.TypeOf(b) || !typ.Comparable() {
		return false
	}
	return a == b
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
func (d *udevDiscovery) newMonitor(ctx context.Context) (<-chan *libudev.Device, <-chan error, error) {
	// go-udev serializes every operation on one Udev context. Use a separate
	// context for monitoring so a potentially slow enumeration cannot prevent
	// the DeviceChan goroutine from draining the netlink socket.
	mon := d.monitorUdev.NewMonitorFromNetlink("udev")

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

	return mon.DeviceChan(ctx)
}

// newMonitorWithRetry opens a monitor, retrying transient setup failures while
// the discovery is alive. Making the delay context-aware keeps Close prompt
// even when udev is unavailable.
func (d *udevDiscovery) newMonitorWithRetry(ctx context.Context) (<-chan *libudev.Device, <-chan error, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		devChan, errChan, err := d.newMonitor(ctx)
		if err == nil {
			return devChan, errChan, nil
		}
		klog.Errorf("Failed to create device channel, retrying: %v", err)

		timer := time.NewTimer(time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, nil, ctx.Err()
		}
	}
}

// enumerationDelta counts key-level additions and removals for diagnostics.
// Metadata changes at a retained syspath are intentionally not classified
// here: every successful pass replaces the Device object and publishes the
// complete snapshot, so downstream projection observes those changes too.
func enumerationDelta(current, next map[Id]Device) (added, removed int) {
	for id := range next {
		if _, ok := current[id]; !ok {
			added++
		}
	}
	for id := range current {
		if _, ok := next[id]; !ok {
			removed++
		}
	}
	return added, removed
}

func deviceSnapshot(state map[Id]Device) []Device {
	devices := make([]Device, 0, len(state))
	for _, dev := range state {
		devices = append(devices, dev)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Id() < devices[j].Id() })
	return devices
}

// recordDeviceAction maintains the exceptional availability overlay carried
// by udev's offline/online actions. Ordinary add/remove membership still comes
// exclusively from enumeration: add/remove only clear a now-obsolete offline
// mark, they do not insert or delete discovery state directly.
func (d *udevDiscovery) recordDeviceAction(action string, id Id) {
	d.mu.Lock()
	switch action {
	case ActionOffline:
		d.actionSequence++
		if d.offline == nil {
			d.offline = make(map[Id]uint64)
		}
		d.offline[id] = d.actionSequence
	case ActionAdd, ActionOnline, ActionRemove:
		d.actionSequence++
		delete(d.offline, id)
	}
	d.mu.Unlock()
}

func (d *udevDiscovery) beginEnumeration() uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.actionSequence
}

// commitEnumeration atomically replaces discovery state with one complete
// enumeration, excluding devices explicitly marked offline. An offline mark
// survives while its ID remains enumerable. An absent ID clears a mark only
// when the mark existed at scanStart: a newer offline event raced with this
// scan and must survive into the event-triggered trailing pass. This ordering
// also lets later authoritative scans garbage-collect marks left behind by a
// missed remove event. Explicit add/online/remove actions clear marks eagerly.
func (d *udevDiscovery) commitEnumeration(found map[Id]Device, scanStart uint64) (Snapshot, int, int) {
	d.mu.Lock()
	for id, actionSequence := range d.offline {
		if _, present := found[id]; !present && actionSequence <= scanStart {
			delete(d.offline, id)
		}
	}

	available := make(map[Id]Device, len(found))
	for id, dev := range found {
		if _, isOffline := d.offline[id]; !isOffline {
			available[id] = dev
		}
	}

	added, removed := enumerationDelta(d.state, available)
	d.state = available
	d.generation++
	snapshot := Snapshot{
		Generation: d.generation,
		Devices:    deviceSnapshot(d.state),
	}
	d.mu.Unlock()

	return snapshot, added, removed
}

// reconcile performs the only discovery-state transition in production. A
// successful enumeration atomically replaces state and publishes one complete
// snapshot. Publishing every successful pass (not only key changes) refreshes
// same-syspath metadata and lets downstream consumers retry failed projection
// without relying on another edge event.
func (d *udevDiscovery) reconcile() bool {
	scanStart := d.beginEnumeration()
	enum := d.udev.NewEnumerate()
	devs, err := enum.Devices()
	if err != nil {
		klog.Errorf("reconcile: failed to enumerate devices: %v", err)
		return false
	}

	found := make(map[Id]Device, len(devs))
	for _, dev := range devs {
		if dev == nil {
			// Treat a nil entry as a failed/partial enumeration. Committing the
			// remainder could turn an observation error into mass removals.
			klog.Error("reconcile: enumeration returned a nil device; retaining previous state")
			return false
		}
		found[Id(dev.Syspath())] = &generic{
			udev: d,
			dev:  dev,
		}
	}

	snapshot, added, removed := d.commitEnumeration(found, scanStart)

	if added > 0 || removed > 0 {
		klog.V(4).Infof("reconcile: generation %d changed device keys: %d additions, %d removals", snapshot.Generation, added, removed)
	}

	if err := d.mux.Submit(snapshot); err != nil {
		klog.Errorf("reconcile: failed to publish snapshot generation %d: %v", snapshot.Generation, err)
	}
	return true
}

// reconcileUntilSuccess obtains the initial authoritative snapshot. Requests
// are not served before this succeeds because there is no last-good state to
// return yet. Once initialized, runMonitor uses a timer-driven retry instead
// so transient failures do not block State or Subscribe.
func (d *udevDiscovery) reconcileUntilSuccess(reconcile func() bool, retryInterval time.Duration) bool {
	for {
		if reconcile() {
			return true
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-timer.C:
		case <-d.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false
		}
	}
}

func (d *udevDiscovery) signalReconcile() {
	select {
	case d.reconcileC <- struct{}{}:
	default:
	}
}

// monitorEvents owns the netlink monitor and continuously drains it while
// enumeration runs in monitor. Event payloads are never applied to membership;
// offline/online lifecycle actions only update the availability overlay before
// marking reconciliation dirty. A capacity-one signal coalesces bursts, and
// an event received during a scan leaves a token for a trailing pass.
func (d *udevDiscovery) monitorEvents(wg *sync.WaitGroup) {
	defer wg.Done()
	defer close(d.eventsDone)

	firstMonitor := true
	for {
		sessionCtx, cancelSession := context.WithCancel(d.ctx)
		devChan, errChan, err := d.newMonitorWithRetry(sessionCtx)
		if err != nil {
			cancelSession()
			return
		}

		if firstMonitor {
			close(d.monitorReady)
			firstMonitor = false
		} else {
			klog.Infof("Successfully reconnected to udev; scheduling reconciliation")
			d.signalReconcile()
		}

	monitorSession:
		for {
			select {
			case <-d.ctx.Done():
				cancelSession()
				return
			case dev, ok := <-devChan:
				if !ok {
					klog.Warning("udev: monitor device channel closed, reconnecting")
					break monitorSession
				}
				if dev == nil {
					klog.Warning("udev: monitor returned a nil device; scheduling reconciliation")
				} else {
					klog.V(5).Infof("Received device notification (%s): %s", dev.Action(), dev.Syspath())
					d.recordDeviceAction(dev.Action(), Id(dev.Syspath()))
				}
				d.signalReconcile()
			case err, ok := <-errChan:
				if !ok {
					klog.Warning("udev: monitor error channel closed, reconnecting")
				} else {
					klog.Errorf("Error from udev monitor, reconnecting: %v", err)
				}
				break monitorSession
			}
		}

		cancelSession()
	}
}

// monitor is the single reconciliation controller. It owns snapshot
// publication and subscription ordering, so a new subscriber's initial replay
// cannot be interleaved with a newer generation.
func (d *udevDiscovery) monitor(wg *sync.WaitGroup) {
	d.runMonitor(wg, d.reconcile, reconcileRetryInterval)
}

// runMonitor is split from monitor so the controller's retry and ordering
// behavior can be tested without a live libudev context.
func (d *udevDiscovery) runMonitor(wg *sync.WaitGroup, reconcile func() bool, retryInterval time.Duration) {
	defer wg.Done()
	defer close(d.done)
	defer d.mux.Close()

	// The monitor socket must be listening before the initial enumeration. Any
	// transition during that scan is then buffered by the pump and schedules a
	// trailing pass.
	select {
	case <-d.monitorReady:
	case <-d.ctx.Done():
		return
	}
	if !d.reconcileUntilSuccess(reconcile, retryInterval) {
		return
	}

	reconcileTicker := time.NewTicker(d.reconcileInterval)
	defer reconcileTicker.Stop()

	var retryTimer *time.Timer
	var retryC <-chan time.Time
	stopRetry := func() {
		if retryTimer == nil {
			return
		}
		if !retryTimer.Stop() {
			select {
			case <-retryTimer.C:
			default:
			}
		}
		retryTimer = nil
		retryC = nil
	}
	defer stopRetry()

	scheduleRetry := func() {
		if retryTimer != nil {
			return
		}
		retryTimer = time.NewTimer(retryInterval)
		retryC = retryTimer.C
	}

	// Every trigger starts one scan immediately. A newer trigger supersedes a
	// scheduled retry; if that scan also fails, its retry interval starts from
	// the latest failure. The controller remains in the select loop throughout
	// the wait and can keep serving the last-good state.
	reconcileOnce := func() {
		stopRetry()
		if !reconcile() {
			scheduleRetry()
		}
	}

	// If the first subscription request races with an event left pending by
	// the initial enumeration (or with an expired retry timer), reconcile once
	// before taking its snapshot. Dirty and retry notifications both describe
	// the same desired operation, so they are coalesced into one bounded scan.
	reconcilePending := func() {
		pending := false
		select {
		case <-d.reconcileC:
			pending = true
		default:
		}
		select {
		case <-retryC:
			retryTimer = nil
			retryC = nil
			pending = true
		default:
		}
		if pending {
			reconcileOnce()
		}
	}

	firstSubscriber := true

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-d.reconcileC:
			reconcileOnce()
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
				if firstSubscriber {
					reconcilePending()
					firstSubscriber = false
				}
				d.mu.RLock()
				snapshot := Snapshot{Generation: d.generation, Devices: deviceSnapshot(d.state)}
				d.mu.RUnlock()
				err := r.sink.Submit(snapshot)
				if err != nil {
					klog.Errorf("Failed to submit initial snapshot: %v", err)
				}
				cancel := d.mux.Subscribe(r.sink)
				req.Reply(cancel)
			}
		case <-reconcileTicker.C:
			reconcileOnce()
		case <-retryC:
			retryTimer = nil
			retryC = nil
			reconcileOnce()
		}
	}
}

func (d *udevDiscovery) Subscribe(sink mux.Sink[Snapshot]) mux.CancelFunc {
	// Full snapshots are level-triggered, so a slow consumer only needs the
	// newest pending generation. This bounded mailbox prevents one scatter from
	// blocking discovery or growing an unbounded delta queue.
	latest := mux.LatestSink(sink)
	await := mux.NewAwaitReply[monitorRequest, any](newSub{latest})
	select {
	case d.requests <- await:
		return await.Await().(mux.CancelFunc)
	case <-d.done:
		latest.Close() // also closes the wrapped sink
		return func() {}
	}
}
