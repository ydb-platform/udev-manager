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

func NetRdmaMatcherInstances(domain string, matcher *regexp.Regexp, resourcesCount int) FromDevice[[]*netRdma] {
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

		rdmaDevice, err := rdmamap.GetRdmaDeviceForNetdevice(ifname)
		if err != nil {
			klog.Errorf("fail to get rdma devices for network device: %s %v", ifname, err)
			return nil, nil
		}
		rdmaCharDevices := rdmamap.GetRdmaCharDevices(rdmaDevice)
		klog.Info("found rdma character devices for ifname: %s devices: %v", ifname, rdmaCharDevices)

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
