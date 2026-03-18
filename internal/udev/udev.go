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

type stopRequest struct{}

func (r stopRequest) requestSealed() {}

type newSub struct {
	sink mux.Sink[Event]
}

func (n newSub) requestSealed() {}

type generic struct {
	udev Discovery

	dev    *libudev.Device
	parent Device
}

func (g *generic) Id() Id {
	return Id(g.dev.Syspath())
}

func (g *generic) Parent() Device {
	if g.parent == nil {
		g.parent = g.udev.DeviceById(Id(g.dev.Syspath()))
	}
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
	if value == "" && g.parent != nil {
		return g.parent.PropertyLookup(key)
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
	if value == "" && g.parent != nil {
		return g.parent.SystemAttributeLookup(key)
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
	s.subscribeC <- subscribeReq{sink: sink, reply: replyCh}
	return <-replyCh
}

type udevDiscovery struct {
	udev     libudev.Udev
	state    map[Id]Device // should be accessed only by monitor goroutine
	requests chan mux.AwaitReply[monitorRequest, any]
	mux      *mux.Mux[Event]
	wg       *sync.WaitGroup
}

// NewDiscovery creates a real udev-backed Discovery. It enumerates all
// currently present devices and starts a monitor goroutine that publishes
// Added and Removed events as the kernel reports them.
func NewDiscovery(wg *sync.WaitGroup) (Discovery, error) {
	d := &udevDiscovery{
		state:    make(map[Id]Device),
		requests: make(chan mux.AwaitReply[monitorRequest, any]),
		mux:      mux.Make[Event](),
		wg:       wg,
	}
	enum := d.udev.NewEnumerate()

	devs, err := enum.Devices()
	if err != nil {
		klog.Errorf("Failed to enumerate devices: %v", err)
		return nil, err
	}

	for _, dev := range devs {
		if dev == nil {
			klog.Error("udev device is nil!")
			continue
		}
		device := &generic{
			udev: d,
			dev:  dev,
		}
		if dev.Parent() != nil {
			device.parent = d.state[Id(dev.Parent().Syspath())]
		}

		d.state[Id(dev.Syspath())] = device
	}

	wg.Add(1)
	go d.monitor(wg)

	return d, nil
}

func (d *udevDiscovery) Close() {
	await := mux.NewAwaitReply[monitorRequest, any](stopRequest{})
	defer await.Await()
	d.requests <- await
}

// State returns the current state of the devices as seen by the monitor
func (d *udevDiscovery) State(filter mux.FilterFunc[Device]) map[Id]Device {
	await := mux.NewAwaitReply[monitorRequest, any](stateRequest{filter: filter})
	d.requests <- await
	return await.Await().(map[Id]Device)
}

func (d *udevDiscovery) DeviceById(id Id) Device {
	return d.state[id] // read-only access should be safe. worst case it would return stale data
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
	}

	evCh := make(chan Event)

	go func() {
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
				if err := req.sink.Submit(sliceSnapshot(slice.state)); err != nil {
					klog.Errorf("slice: failed to replay snapshot to new subscriber: %v", err)
				}
				req.reply <- cancel
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

func (d *udevDiscovery) monitor(wg *sync.WaitGroup) {
	defer wg.Done()
	defer d.mux.Close()
	defer close(d.requests)

	mon := d.udev.NewMonitorFromNetlink("udev")
	devChan, errChan, err := mon.DeviceChan(context.Background())
	if err != nil {
		klog.Errorf("Failed to create device channel: %v", err)
		return
	}

	for {
		select {
		case dev := <-devChan:
			klog.V(5).Infof("Received device event (%s): %s", dev.Action(), dev.Syspath())
			switch dev.Action() {
			case ActionAdd, ActionOnline:
				id := Id(dev.Syspath())
				dev := &generic{
					udev: d,
					dev:  dev,
				}
				d.state[id] = dev
				if err := d.mux.Submit(Added{dev}); err != nil {
					klog.Errorf("udev: failed to submit Added event: %v", err)
				}
			case ActionRemove, ActionOffline:
				id := Id(dev.Syspath())
				dev := d.state[id]
				delete(d.state, id)
				if err := d.mux.Submit(Removed{dev}); err != nil {
					klog.Errorf("udev: failed to submit Removed event: %v", err)
				}
			}
		case req := <-d.requests:
			switch r := req.Value().(type) {
			case stateRequest:
				state := make(map[Id]Device)
				for k, v := range d.state {
					if r.filter(v) {
						state[k] = v
					}
				}
				req.Reply(state)
			case newSub:
				init := make([]Device, 0, len(d.state))
				for _, dev := range d.state {
					init = append(init, dev)
				}
				err := r.sink.Submit(Init{init})
				if err != nil {
					klog.Errorf("Failed to submit init event: %v", err)
				}
				cancel := d.mux.Subscribe(r.sink)
				req.Reply(cancel)
			case stopRequest:
				req.Reply(nil)
				return
			}
		case err := <-errChan:
			klog.Errorf("Error from udev monitor, will try to retry connecting to udev: %v", err)
		retry:
			mon = d.udev.NewMonitorFromNetlink("udev")
			devChan, errChan, err = mon.DeviceChan(context.Background())
			if err != nil {
				klog.Errorf("Failed to create device channel, retrying: %v", err)
				time.Sleep(1 * time.Second)
				goto retry
			}
			klog.Infof("Successfully reconnected to udev")
		}
	}
}

func (d *udevDiscovery) Subscribe(sink mux.Sink[Event]) mux.CancelFunc {
	// here we're doing initialization in monitor goroutine
	// to be able to pass consistent Init event to the sink
	// before making fan out of udev events
	await := mux.NewAwaitReply[monitorRequest, any](newSub{sink})
	d.requests <- await
	return await.Await().(mux.CancelFunc)
}
