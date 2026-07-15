package plugin

import (
	"errors"
	"fmt"

	"k8s.io/klog/v2"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/udev"
)

// DeviceHandler consumes udev devices for one configured resource type.
// Initial devices are staged one at a time and are not published to kubelet
// until InitComplete is called.
type DeviceHandler interface {
	InitDevice(udev.Device) error
	InitComplete() error
	Added(udev.Device) error
	Removed(udev.Device) error
}

// RunDeviceHandlers subscribes to discovery exactly once and dispatches its
// events to every configured handler. Initial discovery has two phases:
//
//  1. Every device is offered to every handler, building a complete snapshot.
//  2. InitComplete publishes the staged resources to kubelet.
//
// onInitialized is called after both phases finish. A non-nil error means at
// least one device could not be processed or one resource could not be
// registered, and callers should keep readiness false.
func RunDeviceHandlers(
	discovery udev.Discovery,
	onInitialized func(error),
	handlers ...DeviceHandler,
) mux.CancelFunc {
	events := make(chan udev.Event, 64)
	go runDeviceHandlers(events, onInitialized, handlers)
	return discovery.Subscribe(mux.SinkFromChan(events))
}

func runDeviceHandlers(
	events <-chan udev.Event,
	onInitialized func(error),
	handlers []DeviceHandler,
) {
	initialized := false
	defer func() {
		if !initialized {
			err := errors.New("udev discovery closed before the initial snapshot completed")
			if onInitialized != nil {
				onInitialized(err)
			} else {
				klog.Error(err)
			}
		}
	}()

	for event := range events {
		switch event := event.(type) {
		case udev.Init:
			if initialized {
				klog.Error("received more than one udev Init event")
				continue
			}

			var initErrors []error
			for _, dev := range event.Devices {
				for i, handler := range handlers {
					if err := handler.InitDevice(dev); err != nil {
						initErrors = append(initErrors, fmt.Errorf(
							"handler %d failed to initialize device %q: %w",
							i, deviceID(dev), err,
						))
					}
				}
			}

			// No handler can publish a resource until all handlers have seen the
			// complete initial device snapshot.
			for i, handler := range handlers {
				if err := handler.InitComplete(); err != nil {
					initErrors = append(initErrors, fmt.Errorf(
						"handler %d failed to publish initial resources: %w", i, err,
					))
				}
			}

			initialized = true
			initErr := errors.Join(initErrors...)
			if onInitialized != nil {
				onInitialized(initErr)
			} else if initErr != nil {
				klog.Errorf("initial device discovery or registration failed: %v", initErr)
			}

		case udev.Added:
			for i, handler := range handlers {
				if err := handler.Added(event.Device); err != nil {
					klog.Errorf("handler %d failed to add device %q: %v", i, deviceID(event.Device), err)
				}
			}

		case udev.Removed:
			for i, handler := range handlers {
				if err := handler.Removed(event.Device); err != nil {
					klog.Errorf("handler %d failed to remove device %q: %v", i, deviceID(event.Device), err)
				}
			}
		}
	}
}

func deviceID(dev udev.Device) udev.Id {
	if dev == nil {
		return ""
	}
	return dev.Id()
}
