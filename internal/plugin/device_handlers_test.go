package plugin

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"
)

type countingDiscovery struct {
	udev.Discovery
	subscriptions int
}

func (d *countingDiscovery) Subscribe(sink mux.Sink[udev.Event]) mux.CancelFunc {
	d.subscriptions++
	return d.Discovery.Subscribe(sink)
}

type recordingDeviceHandler struct {
	name        string
	calls       *[]string
	initErr     error
	completeErr error
}

func (h *recordingDeviceHandler) InitDevice(dev udev.Device) error {
	*h.calls = append(*h.calls, h.name+":init:"+string(deviceID(dev)))
	return h.initErr
}

func (h *recordingDeviceHandler) InitComplete() error {
	*h.calls = append(*h.calls, h.name+":complete")
	return h.completeErr
}

func (h *recordingDeviceHandler) Added(dev udev.Device) error {
	*h.calls = append(*h.calls, h.name+":add:"+string(deviceID(dev)))
	return nil
}

func (h *recordingDeviceHandler) Removed(dev udev.Device) error {
	*h.calls = append(*h.calls, h.name+":remove:"+string(deviceID(dev)))
	return nil
}

var _ = Describe("device handler dispatcher", func() {
	It("uses one discovery subscription for every configured handler", func() {
		base := udev.NewFakeDiscovery()
		DeferCleanup(base.Close)
		discovery := &countingDiscovery{Discovery: base}
		var calls []string
		h1 := &recordingDeviceHandler{name: "one", calls: &calls}
		h2 := &recordingDeviceHandler{name: "two", calls: &calls}
		initialized := make(chan error, 1)

		cancel := RunDeviceHandlers(discovery, func(err error) { initialized <- err }, h1, h2)
		DeferCleanup(cancel)

		Eventually(initialized).Should(Receive(BeNil()))
		Expect(discovery.subscriptions).To(Equal(1))
	})

	It("stages every device in every handler before publishing resources", func() {
		var calls []string
		h1 := &recordingDeviceHandler{name: "one", calls: &calls}
		h2 := &recordingDeviceHandler{name: "two", calls: &calls}
		initialized := make(chan error, 1)
		events := make(chan udev.Event, 1)

		go runDeviceHandlers(events, func(err error) { initialized <- err }, []DeviceHandler{h1, h2})
		events <- udev.Init{Devices: []udev.Device{
			udev.NewFakeDevice("disk-1"),
			udev.NewFakeDevice("disk-2"),
		}}

		Eventually(initialized).Should(Receive(BeNil()))
		Expect(calls).To(Equal([]string{
			"one:init:disk-1",
			"two:init:disk-1",
			"one:init:disk-2",
			"two:init:disk-2",
			"one:complete",
			"two:complete",
		}))
		close(events)
	})

	It("reports processing and registration failures to the readiness callback", func() {
		var calls []string
		h := &recordingDeviceHandler{
			name:        "broken",
			calls:       &calls,
			initErr:     errors.New("device failed"),
			completeErr: errors.New("registration failed"),
		}
		initialized := make(chan error, 1)
		events := make(chan udev.Event, 1)

		go runDeviceHandlers(events, func(err error) { initialized <- err }, []DeviceHandler{h})
		events <- udev.Init{Devices: []udev.Device{udev.NewFakeDevice("disk-1")}}

		var err error
		Eventually(initialized).Should(Receive(&err))
		Expect(err).To(MatchError(And(
			ContainSubstring("device failed"),
			ContainSubstring("registration failed"),
		)))
		close(events)
	})

	It("does not report ready when discovery closes before Init", func() {
		initialized := make(chan error, 1)
		events := make(chan udev.Event)
		go runDeviceHandlers(events, func(err error) { initialized <- err }, nil)

		close(events)

		var err error
		Eventually(initialized).Should(Receive(&err))
		Expect(err).To(MatchError(ContainSubstring("before the initial snapshot completed")))
	})
})
