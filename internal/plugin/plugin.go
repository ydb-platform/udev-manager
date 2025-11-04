package plugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/kennygrant/sanitize"

	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type plugin struct {
	resource Resource
	cancel   context.CancelFunc
}

func newPlugin(resource Resource, ctx context.Context, wg *sync.WaitGroup) (*plugin, error) {
	ctx, cancel := context.WithCancel(ctx)
	plugin := &plugin{
		resource: resource,
		cancel:   cancel,
	}

	socketPath := pluginapi.DevicePluginPath + plugin.socketPath()
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		klog.Errorf("%q: failed to remove socket file %q: %v", plugin.resource.Name(), socketPath, err)
		return nil, fmt.Errorf("failed to remove socket file %s: %w", socketPath, err)
	}

	server := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(server, plugin)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		klog.Errorf("%q: failed to listen on socket %q: %v", plugin.resource.Name(), socketPath, err)
		return nil, fmt.Errorf("failed to listen on socket %s: %w", socketPath, err)
	}

	go server.Serve(listener)

	wg.Add(1)
	go func(ctx context.Context, wg *sync.WaitGroup, l net.Listener, s *grpc.Server) {
		defer wg.Done()
		defer l.Close()
		defer s.Stop()
		klog.Infof("Serving device plugin %q on socket %q", plugin.resource.Name(), socketPath)
		<-ctx.Done()
	}(ctx, wg, listener, server)

	return plugin, nil
}

func (p *plugin) stop() {
	if p.cancel != nil {
		klog.Infof("%q: Stopping device plugin", p.resource.Name())
		p.cancel()
	}
}

func (p *plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

func (p *plugin) GetPreferredAllocation(context.Context, *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

func (p *plugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (p *plugin) ListAndWatch(empty *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) (err error) {
	defer klog.Infof("%q: closing ListAndWatch connection, err = %v", p.resource.Name(), err)

	for instances := range p.resource.ListAndWatch(stream.Context()) {
		devices := make([]*pluginapi.Device, len(instances))
		for i, instance := range instances {
			devices[i] = &pluginapi.Device{
				ID:       string(instance.Id()),
				Health:   instance.Health().String(),
				Topology: instance.TopologyHints(),
			}
		}
		klog.V(2).Infof("%q: sending devices to ListAndWatch stream: %+v", p.resource.Name(), devices)
		if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: devices}); err != nil {
			klog.Errorf("%q: failed to send devices to ListAndWatch stream: %v", p.resource.Name(), err)
			return err
		}
	}
	return nil
}

func mergeResponses(responses ...*pluginapi.ContainerAllocateResponse) *pluginapi.ContainerAllocateResponse {
	response := &pluginapi.ContainerAllocateResponse{}
	for _, r := range responses {
		if r == nil {
			continue
		}
		response.Devices = append(response.Devices, r.Devices...)
		response.Mounts = append(response.Mounts, r.Mounts...)
		if len(r.Envs) > 0 {
			if response.Envs == nil {
				response.Envs = make(map[string]string)
			}
			for key, value := range r.Envs {
				response.Envs[key] = value
			}
		}
		if len(r.Annotations) > 0 {
			if response.Annotations == nil {
				response.Annotations = make(map[string]string)
			}
			for key, value := range r.Annotations {
				response.Annotations[key] = value
			}
		}
	}
	return response
}

func (p *plugin) Allocate(ctx context.Context, request *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	klog.Infof("%q: Received allocation request", p.resource.Name())
	klog.V(2).Infof("%+v", request)

	instances := p.resource.Instances()
	response := &pluginapi.AllocateResponse{}
	for _, containerRequest := range request.ContainerRequests {
		containerResponse := &pluginapi.ContainerAllocateResponse{}
		klog.V(2).Infof("%q: Processing container request: %+v", p.resource.Name(), containerRequest)
		for _, id := range containerRequest.DevicesIDs {
			instance, found := instances[Id(id)]
			if !found {
				klog.Errorf("%q: device with ID %q not found", p.resource.Name(), id)
				return nil, status.Errorf(codes.NotFound, "device with ID %q not found", id)
			}
			allocateResponse, err := instance.Allocate(ctx)
			if err != nil {
				klog.Errorf("%q: failed to allocate device with ID %q: %v", p.resource.Name(), id, err)
				return nil, status.Errorf(codes.Internal, "failed to allocate device with ID %q: %s", id, err.Error())
			}
			containerResponse = mergeResponses(containerResponse, allocateResponse)
		}
		response.ContainerResponses = append(response.ContainerResponses, containerResponse)
	}

	klog.V(2).Infof("%q: Responding to allocation request with: %+v", p.resource.Name(), response)
	return response, nil
}

func (p *plugin) socketPath() string {
	return sanitize.BaseName(p.resource.Name()) + ".sock"
}

func (p *plugin) probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	pluginAddr := "unix://" + pluginapi.DevicePluginPath + p.socketPath()

	conn, err := grpc.DialContext(
		ctx,
		pluginAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		klog.Errorf("%q: failed to dial %q: %v", p.resource.Name(), pluginAddr, err)
		return fmt.Errorf("failed to dial %q: %w", pluginAddr, err)
	}
	defer conn.Close()

	client := pluginapi.NewDevicePluginClient(conn)
	_, err = client.GetDevicePluginOptions(context.Background(), &pluginapi.Empty{})

	if err != nil {
		klog.Errorf("%q: failed to get device plugin options: %v", p.resource.Name(), err)
		return fmt.Errorf("plugin[%q]: failed to get device plugin options: %w", p.resource.Name(), err)
	}

	return nil
}
