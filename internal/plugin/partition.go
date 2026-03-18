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
	domain               string
	label                string
	dev                  udev.Device
	disableTopologyHints bool
}

func (p *partition) Id() Id {
	return Id(p.label)
}

func (p *partition) Health() Health {
	return Healthy{} //TODO: health check?
}

func (p *partition) TopologyHints() *pluginapi.TopologyInfo {
	if p.disableTopologyHints {
		return nil
	}
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

func (p *partition) Allocate(context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	response := allocatePartitionDevice(p.dev, p.domain, p.label)
	klog.Info("allocated partition: ", p.label)
	klog.V(2).Infof("%+v", response)
	return response, nil
}

func allocatePartitionDevice(dev udev.Device, domain, label string) *pluginapi.ContainerAllocateResponse {
	response := &pluginapi.ContainerAllocateResponse{}

	containerPath := path.Join("/dev", "allocated", domain, "part", label)

	response.Devices = append(response.Devices, &pluginapi.DeviceSpec{
		HostPath:      dev.DevNode(),
		ContainerPath: containerPath,
		Permissions:   "rw",
	})

	envName := func(env string) string {
		return sanitizeEnv(domain) + "_PART_" + sanitizeEnv(label) + "_" + sanitizeEnv(env)
	}

	response.Envs = make(map[string]string)
	response.Envs[envName("PATH")] = containerPath
	response.Envs[envName("DISK_ID")] =
		dev.SystemAttributeLookup(udev.SysAttrWWID)
	response.Envs[envName("DISK_MODEL")] =
		dev.SystemAttributeLookup(udev.SysAttrModel)

	serial := dev.SystemAttributeLookup(udev.SysAttrSerial)
	if serial == "" {
		serial = dev.PropertyLookup(udev.PropertyShortSerial)
	}
	if serial != "" {
		response.Envs[envName("DISK_SERIAL")] = serial
	}

	return response
}

// PartitionLabelMatcherTemplater returns a FromDevice function that produces a
// ResourceTemplate for block partition devices whose PARTNAME matches matcher.
// The first capture group (if any) is used as the label suffix.
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

// PartitionLabelMatcherInstances returns a FromDevice function that produces
// partition instances for matching block devices. Each matching device becomes
// one instance whose label is the first capture group of matcher (or the full
// PARTNAME if there are no capture groups).
func PartitionLabelMatcherInstances(domain string, matcher *regexp.Regexp, disableTopologyHints bool) FromDevice[[]*partition] {
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
			label:                partlabel,
			domain:               domain,
			dev:                  dev,
			disableTopologyHints: disableTopologyHints,
		}

		return []*partition{part}, nil
	}
}
