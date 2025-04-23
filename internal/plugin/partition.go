package plugin

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/ydb-platform/udev-manager/internal/udev"

	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Partition represents a partition of a disk as a Resource.
type partition struct {
	domain string
	label  string
	dev    udev.Device
}

func (p *partition) Id() Id {
	return Id(p.label)
}

func (p *partition) Health() Health {
	return Healthy{} //TODO: health check?
}

func (p *partition) TopologyHints() *pluginapi.TopologyInfo {
	numaNode := p.dev.NumaNode()
	if numaNode < 0 {
		return nil
	}

	return &pluginapi.TopologyInfo{
		Nodes: []*pluginapi.NUMANode{
			{
				ID: int64(numaNode),
			},
		},
	}
}

func sanitizeEnv(s string) string {
	replacer := strings.NewReplacer(".", "_", "-", "_")
	return strings.ToUpper(replacer.Replace(s))
}

func (p *partition) envName(env string) string {
	return sanitizeEnv(p.domain) + "_PART_" + sanitizeEnv(p.label) + "_" + sanitizeEnv(env)
}

func (p *partition) Allocate(context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	response := &pluginapi.ContainerAllocateResponse{}

	containerPath := path.Join("/dev", "allocated", p.domain, "part", p.label)

	response.Devices = append(response.Devices, &pluginapi.DeviceSpec{
		HostPath:      p.dev.DevNode(),
		ContainerPath: containerPath,
		Permissions:   "rw",
	})

	response.Envs = make(map[string]string)
	response.Envs[p.envName("PATH")] = containerPath
	response.Envs[p.envName("DISK_ID")] =
		p.dev.SystemAttributeLookup(udev.SysAttrWWID)
	response.Envs[p.envName("DISK_MODEL")] =
		p.dev.SystemAttributeLookup(udev.SysAttrModel)

	serial := p.dev.SystemAttributeLookup(udev.SysAttrSerial)
	if serial == "" {
		serial = p.dev.PropertyLookup(udev.PropertyShortSerial)
	}
	if serial != "" {
		response.Envs[p.envName("DISK_SERIAL")] = serial
	}

	klog.Info("allocated partition: ", p.label)
	klog.V(2).Infof("%+v", response)

	return response, nil
}

func PartitionLabelMatcherTemplater(domain string, matcher *regexp.Regexp) FromDevice[*ResourceTemplate] {
	return func(dev udev.Device) (*ResourceTemplate, error) {
		if dev.Subsystem() != udev.BlockSubsystem {
			return nil, nil
		}
		if dev.DevType() != udev.DeviceTypePart {
			return nil, nil
		}

		partlabel, found := dev.Properties()[udev.PropertyPartName]
		if !found {
			return nil, nil
		}

		matches := matcher.FindStringSubmatch(partlabel)
		if len(matches) == 0 {
			return nil, nil
		}

		partlabel = strings.Join(matches[1:], "_")

		return &ResourceTemplate{
			Domain: domain,
			Prefix: fmt.Sprintf("part-%s", partlabel),
		}, nil
	}
}

func PartitionLabelMatcherInstances(domain string, matcher *regexp.Regexp) FromDevice[[]*partition] {
	return func(dev udev.Device) ([]*partition, error) {
		var part *partition
		if dev.Subsystem() != udev.BlockSubsystem {
			return nil, nil
		}
		if dev.DevType() != udev.DeviceTypePart {
			return nil, nil
		}

		partlabel, found := dev.Properties()[udev.PropertyPartName]
		if !found {
			return nil, nil
		}

		matches := matcher.FindStringSubmatch(partlabel)
		if len(matches) == 0 {
			return nil, nil
		}

		if len(matches) > 1 {
			partlabel = matches[1]
		}

		part = &partition{
			label:  partlabel,
			domain: domain,
			dev:    dev,
		}

		return []*partition{part}, nil
	}
}
