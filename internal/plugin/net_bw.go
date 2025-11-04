package plugin

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ydb-platform/udev-manager/internal/udev"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type networkBandwidth struct {
	ifname string
	idx    int
	dev    udev.Device
}

func (n *networkBandwidth) Id() Id {
	return Id(fmt.Sprintf("%s_%d", n.ifname, n.idx))
}

func (n *networkBandwidth) Health() Health {
	if n.dev.SystemAttribute(udev.SysAttrOperstate) == "up" {
		return Healthy{}
	}
	return Unhealthy{}
}

func (n *networkBandwidth) TopologyHints() *pluginapi.TopologyInfo {
	return nil
}

func (n *networkBandwidth) Allocate(context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	response := &pluginapi.ContainerAllocateResponse{}

	return response, nil
}

func NetBWMatcherTemplater(domain string, matcher *regexp.Regexp) FromDevice[*ResourceTemplate] {
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
			Prefix: fmt.Sprintf("netbw-%s", ifname),
		}, nil
	}
}

func NetBWMatcherInstances(domain string, matcher *regexp.Regexp, mbpsPerShare uint) FromDevice[[]*networkBandwidth] {
	return func(dev udev.Device) ([]*networkBandwidth, error) {
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

		speedString := dev.SystemAttribute(udev.SysAttrSpeed)
		if speedString == "" {
			return nil, nil
		}

		speedMbps, err := strconv.Atoi(speedString)
		if err != nil {
			return nil, nil
		}

		shares := speedMbps / int(mbpsPerShare)
		if shares == 0 {
			return nil, nil
		}

		instances := make([]*networkBandwidth, 0, shares)
		for i := 0; i < shares; i++ {
			instances = append(instances, &networkBandwidth{
				ifname: ifname,
				idx:    i,
				dev:    dev,
			})
		}

		return instances, nil
	}
}
