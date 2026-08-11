package plugin

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Mellanox/rdmamap"
	"github.com/ydb-platform/udev-manager/internal/udev"
	"k8s.io/klog/v2"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type netRdma struct {
	domain            string
	ifname            string
	idx               int
	dev               udev.Device
	associatedDevices []string
}

func (n *netRdma) Id() Id {
	return Id(fmt.Sprintf("%s_%d", n.ifname, n.idx))
}

func (n *netRdma) Health() Health {
	if n.dev.SystemAttribute(udev.SysAttrOperstate) == "up" {
		return Healthy{}
	}
	return Unhealthy{}
}

func (n *netRdma) TopologyHints() *pluginapi.TopologyInfo {
	return nil
}

func (n *netRdma) Allocate(context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	response := &pluginapi.ContainerAllocateResponse{}

	for _, dev := range n.associatedDevices {
		response.Devices = append(response.Devices, &pluginapi.DeviceSpec{
			HostPath:      dev,
			ContainerPath: dev,
			Permissions:   "rw",
		})
	}
	klog.V(2).Infof("%+v", response)

	return response, nil

}

// NetRdmaMatcherTemplater returns a FromDevice function that produces a
// ResourceTemplate for RDMA-capable net devices whose INTERFACE matches
// matcher.
func NetRdmaMatcherTemplater(domain string, matcher *regexp.Regexp) FromDevice[*ResourceTemplate] {
	return func(dev udev.Device) (*ResourceTemplate, error) {
		if dev.Subsystem() != udev.NetSubsystem {
			return nil, nil
		}

		ifname := dev.Property(udev.PropertyInterface)
		if ifname == "" {
			return nil, nil
		}

		matches := matcher.FindStringSubmatch(ifname)
		if len(matches) == 0 {
			return nil, nil
		}

		ifname = strings.Join(matches[1:], "_")

		return &ResourceTemplate{
			Domain: domain,
			Prefix: fmt.Sprintf("netrdma-%s", ifname),
		}, nil
	}
}

// NetRdmaMatcherInstances returns a FromDevice function that produces
// resourcesCount netRdma instances for each matching RDMA-capable net device.
func NetRdmaMatcherInstances(domain string, matcher *regexp.Regexp, resourcesCount int) FromDevice[[]*netRdma] {
	return netRdmaMatcherInstances(
		domain,
		matcher,
		resourcesCount,
		rdmamap.GetRdmaDeviceForNetdevice,
		rdmamap.GetRdmaCharDevices,
	)
}

func netRdmaMatcherInstances(
	domain string,
	matcher *regexp.Regexp,
	resourcesCount int,
	lookupDevice func(string) (string, error),
	lookupCharDevices func(string) []string,
) FromDevice[[]*netRdma] {
	return func(dev udev.Device) ([]*netRdma, error) {
		if dev.Subsystem() != udev.NetSubsystem {
			return nil, nil
		}

		ifname := dev.Property(udev.PropertyInterface)
		if ifname == "" {
			return nil, nil
		}

		if !matcher.MatchString(ifname) {
			return nil, nil
		}

		rdmaDevice, err := lookupDevice(ifname)
		if isRdmaNonMatch(ifname, rdmaDevice, err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("get RDMA device for network interface %q: %w", ifname, err)
		}
		rdmaCharDevices := lookupCharDevices(rdmaDevice)
		klog.Infof("found rdma character devices for ifname: %s devices: %v", ifname, rdmaCharDevices)

		instances := make([]*netRdma, 0, resourcesCount)
		for i := 0; i < resourcesCount; i++ {
			instances = append(instances, &netRdma{
				domain:            domain,
				ifname:            ifname,
				idx:               i,
				dev:               dev,
				associatedDevices: rdmaCharDevices,
			})
		}

		return instances, nil
	}
}

func isRdmaNonMatch(ifname, rdmaDevice string, err error) bool {
	if err == nil {
		// The upstream lookup returns an empty device without an error when an
		// IPoIB interface has no matching RDMA device.
		return rdmaDevice == ""
	}

	// rdmamap does not export sentinel errors. These two exact errors describe
	// definitive non-RDMA interfaces; LinkByName and sysfs read errors remain
	// transient failures so Scatter preserves their last-good resource state.
	return err.Error() == fmt.Sprintf("rdma device not found for netdev %s", ifname) ||
		err.Error() == "unknown device type"
}
