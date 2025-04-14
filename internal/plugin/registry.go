package plugin

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Registry is a lifecycle manager for plugins.
// It is responsible for (re-)registering plugins with the kubelet.
type Registry struct {
	plugins sync.Map
	ctx    	context.Context
	wg      *sync.WaitGroup
	watcher *fsnotify.Watcher
}

const kubeletAddr = "unix://" + pluginapi.KubeletSocket

// register advertises plugin socket to the kubelet.
func (r *Registry) register(plugin *plugin) error {
	conn, err := grpc.Dial(kubeletAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		klog.Errorf("failed to dial %q: %v", kubeletAddr, err)
		return err
	}
	defer func() {
		err := conn.Close()
		if err != nil {
			klog.Errorf("failed to close connection: %v", err)
		}
	}()

	client := pluginapi.NewRegistrationClient(conn)

	_, err = client.Register(context.Background(), &pluginapi.RegisterRequest{
		ResourceName: plugin.resource.Name(),
		Version:      pluginapi.Version,
		Endpoint:     plugin.socketPath(),
		Options:      &pluginapi.DevicePluginOptions{},
	})
	if err != nil {
		klog.Infof("failed to register with kubelet: %v", err)
		return fmt.Errorf("failed to register with kubelet: %w", err)
	}
	klog.Infof("registered device %s with kubelet", plugin.resource.Name())
	return nil
}

// hup registers all plugins with the freshly kubelet.
// Newely started kubelet removes all socket files, so we need to re-register
// all plugins. See https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/#handling-kubelet-restarts
func (r *Registry) hup() {
	r.plugins.Range(func(_, p interface{}) bool {
		plugin := p.(*plugin)
		plugin.stop()
		plugin, err := newPlugin(plugin.resource, r.ctx, r.wg)
		if err != nil {
			klog.Errorf("failed to create plugin for %s: %v", plugin.resource.Name(), err)
			return true
		}
		if err := r.register(plugin); err != nil {
			klog.Errorf("failed to register %s: %v", plugin.resource.Name(), err)
		}
		return true
	})
}

// NewRegistry creates a new registry and starts goroutine
// that watches for kubelet restarts. Whenever kubelet restarts,
// the registry will re-register all plugins.
// `ctx`: context that controls the lifecycle of the registry.
// `wg`: wait group that would be waited on before the registry is stopped.
func NewRegistry(ctx context.Context, wg *sync.WaitGroup) (*Registry, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		klog.Errorf("failed to create fsnotifier watcher: %v", err)
		return nil, fmt.Errorf("failed to create fsnotifier watcher: %w", err)
	}

	registry := &Registry{
		plugins: sync.Map{},
		wg:      wg,
		ctx:     ctx,
		watcher: watcher,
	}

	watcher.Add(path.Join(pluginapi.KubeletSocket))

	registry.wg.Add(1)
	go func(r *Registry) {
		defer r.wg.Done()
		defer r.watcher.Close()

		for {
			select {
			case event := <-r.watcher.Events:
				if event.Op & fsnotify.Create != 0 {
					r.hup()
				}
			case <-r.ctx.Done():
				// Parent context is done, exit the goroutine.
				return
			}
		}
	}(registry)	

	return registry, nil
}

func (r *Registry) Healthz(resp http.ResponseWriter, req *http.Request) {
	unhealthy := make([]string, 0)
	r.plugins.Range(func(_, p interface{}) bool {
		plugin := p.(*plugin)
		if err := plugin.probe(req.Context()); err != nil {
			klog.Errorf("probe failed for %s: %v", plugin.resource.Name(), err)
			unhealthy = append(unhealthy, plugin.resource.Name())
		} else {
			klog.V(2).Infof("probe succeeded for %s", plugin.resource.Name())
		}
		return true
	})

	if len(unhealthy) == 0 {
		resp.WriteHeader(http.StatusOK)
	} else {
		resp.WriteHeader(http.StatusInternalServerError)
		for _, name := range unhealthy {
			fmt.Fprintf(resp, "probe failed for device plugin %q\n", name)
		}
	}
}

// Add creates a new plugin for given Resource and registers it with the
// kubelet. Attempts to register resource with the same name twice will result
// in an error.
func (r *Registry) Add(resource Resource) error {
	plugin, err := newPlugin(resource, r.ctx, r.wg)
	if err != nil {
		klog.Errorf("failed to create plugin for resource %q Cause: %v", resource.Name(), err)
		return err
	}

	_, loaded := r.plugins.LoadOrStore(resource.Name(), plugin)
	if loaded {
		klog.Errorf("resource with name %q already exists", resource.Name())
		return fmt.Errorf("resource with name %q already exists", resource.Name())
	}
	if err := r.register(plugin); err != nil {
		klog.Errorf("failed to register resource %q Cause: %v", resource.Name(), err)
		return err
	}
	return nil
}


