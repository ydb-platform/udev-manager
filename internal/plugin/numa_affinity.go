package plugin

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// numaAffinityDevice is a statically-configured, allocatable slot whose only
// purpose is to advertise a NUMA topology hint to the kubelet. It is not backed
// by any real udev device: allocating it mounts nothing and sets no env vars,
// it simply pins the requesting container to the configured NUMA node via the
// kubelet's topology manager.
type numaAffinityDevice struct {
	id       Id
	numaNode int
}

func (n *numaAffinityDevice) Id() Id { return n.id }

// Health is always Healthy: the device is purely virtual and configuration
// driven, so it is available as long as the resource exists.
func (n *numaAffinityDevice) Health() Health { return Healthy{} }

func (n *numaAffinityDevice) TopologyHints() *pluginapi.TopologyInfo {
	return &pluginapi.TopologyInfo{
		Nodes: []*pluginapi.NUMANode{
			{
				ID: int64(n.numaNode),
			},
		},
	}
}

// Allocate returns an empty response: there is no real device to expose, the
// NUMA affinity is communicated through TopologyHints instead.
func (n *numaAffinityDevice) Allocate(context.Context) (*pluginapi.ContainerAllocateResponse, error) {
	return &pluginapi.ContainerAllocateResponse{}, nil
}

// NewNumaAffinityResource creates and registers a resource named
// "{domain}/numa-{name}" exposing count allocatable devices, all reporting
// affinity to the given NUMA node. Unlike the other resource types it does not
// subscribe to udev discovery: the devices are static and healthy from the
// moment the resource is registered.
func NewNumaAffinityResource(
	registry *Registry,
	domain string,
	name string,
	numaNode int,
	count int,
) error {
	instanceMap := make(map[Id]Instance, count)
	for i := 0; i < count; i++ {
		dev := &numaAffinityDevice{
			id:       Id(fmt.Sprintf("%d", i)),
			numaNode: numaNode,
		}
		instanceMap[dev.id] = dev
	}

	res := newResource(ResourceTemplate{
		Domain: domain,
		Prefix: "numa-" + name,
	}, instanceMap)

	if err := registry.Add(res); err != nil {
		klog.Errorf("failed to add numa affinity resource %s: %v", res.Name(), err)
		res.Close()
		return err
	}

	return nil
}
