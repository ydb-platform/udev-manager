package plugin

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// reregisterDelay spaces out re-registration after a broken ListAndWatch
// stream, avoiding a tight register/drop loop against an unhealthy kubelet.
const reregisterDelay = time.Second

// Registry is a lifecycle manager for plugins.
// It is responsible for (re-)registering plugins with the kubelet.
type Registry struct {
	plugins       sync.Map
	ctx           context.Context
	wg            *sync.WaitGroup
	watcher       *fsnotify.Watcher
	pluginDir     string
	kubeletSocket string
}

// RegistryOption configures a [Registry] created by [NewRegistry].
type RegistryOption func(*Registry)

// WithPluginDir overrides the directory where plugin Unix sockets are created.
// Defaults to pluginapi.DevicePluginPath.
func WithPluginDir(dir string) RegistryOption {
	return func(r *Registry) { r.pluginDir = dir }
}

// WithKubeletSocket overrides the path to the kubelet registration socket.
// Defaults to pluginapi.KubeletSocket.
func WithKubeletSocket(socketPath string) RegistryOption {
	return func(r *Registry) { r.kubeletSocket = socketPath }
}

// register advertises plugin socket to the kubelet.
func (r *Registry) register(plugin *plugin) error {
	addr := "unix://" + r.kubeletSocket
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		klog.Errorf("failed to dial %q: %v", addr, err)
		return err
	}
	defer func() {
		err := conn.Close()
		if err != nil {
			klog.Errorf("failed to close connection: %v", err)
		}
	}()

	client := pluginapi.NewRegistrationClient(conn)

	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()

	_, err = client.Register(ctx, &pluginapi.RegisterRequest{
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

// reregister re-registers a plugin with the kubelet after its ListAndWatch
// stream was broken. The kubelet never re-opens a broken stream on its own;
// it only opens a new one in response to a Register call. Runs asynchronously
// because it is invoked from the ListAndWatch handler itself.
func (r *Registry) reregister(p *plugin) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			select {
			case <-time.After(reregisterDelay):
			case <-p.stopped:
				// The plugin was stopped (e.g. kubelet restart triggered hup);
				// the replacement plugin registers itself.
				return
			case <-r.ctx.Done():
				return
			}
			klog.Warningf("%s: ListAndWatch stream broken by kubelet; re-registering", p.resource.Name())
			if err := r.register(p); err == nil {
				return
			}
			// register failed; loop and retry after another delay.
		}
	}()
}

// hup registers all plugins with the freshly kubelet.
// Newely started kubelet removes all socket files, so we need to re-register
// all plugins. See https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/#handling-kubelet-restarts
func (r *Registry) hup() {
	r.plugins.Range(func(key, p interface{}) bool {
		old := p.(*plugin)
		old.stop()
		newP, err := newPlugin(old.resource, r.ctx, r.wg, r.pluginDir, r.reregister)
		if err != nil {
			klog.Errorf("failed to create plugin for %s: %v", old.resource.Name(), err)
			return true
		}
		r.plugins.Store(key, newP)
		if err := r.register(newP); err != nil {
			klog.Errorf("failed to register %s: %v", newP.resource.Name(), err)
		}
		return true
	})
}

// NewRegistry creates a new registry and starts goroutine
// that watches for kubelet restarts. Whenever kubelet restarts,
// the registry will re-register all plugins.
// `ctx`: context that controls the lifecycle of the registry.
// `wg`: wait group that would be waited on before the registry is stopped.
func NewRegistry(ctx context.Context, wg *sync.WaitGroup, opts ...RegistryOption) (*Registry, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		klog.Errorf("failed to create fsnotifier watcher: %v", err)
		return nil, fmt.Errorf("failed to create fsnotifier watcher: %w", err)
	}

	registry := &Registry{
		plugins:       sync.Map{},
		wg:            wg,
		ctx:           ctx,
		watcher:       watcher,
		pluginDir:     pluginapi.DevicePluginPath,
		kubeletSocket: pluginapi.KubeletSocket,
	}

	for _, opt := range opts {
		opt(registry)
	}

	// Watch the parent directory of the kubelet socket, not the socket file
	// itself. When kubelet restarts it removes and re-creates the socket;
	// watching the file directly loses the inotify watch on deletion and
	// never sees the subsequent CREATE.
	kubeletSocketDir := path.Dir(registry.kubeletSocket)
	if err := watcher.Add(kubeletSocketDir); err != nil {
		if closeErr := watcher.Close(); closeErr != nil {
			klog.Errorf("failed to close watcher: %v", closeErr)
		}
		return nil, fmt.Errorf("failed to watch kubelet socket dir %q: %w", kubeletSocketDir, err)
	}

	registry.wg.Add(1)
	go func(r *Registry) {
		defer r.wg.Done()
		defer func() {
			if err := r.watcher.Close(); err != nil {
				klog.Errorf("failed to close watcher: %v", err)
			}
		}()

		for {
			select {
			case event, ok := <-r.watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Create != 0 && event.Name == r.kubeletSocket {
					r.hup()
				}
			case err, ok := <-r.watcher.Errors:
				if !ok {
					return
				}
				// An error here (typically an inotify queue overflow) means
				// events were lost — possibly the kubelet socket CREATE.
				// Leaving this channel undrained would wedge the watcher
				// entirely. Resync by re-registering everything.
				klog.Errorf("kubelet socket watcher error, re-registering all plugins: %v", err)
				r.hup()
			case <-r.ctx.Done():
				// Parent context is done, exit the goroutine.
				return
			}
		}
	}(registry)

	return registry, nil
}

// Healthz is an HTTP handler that reports the health of all registered device
// plugins. It returns 200 OK if all plugins pass their probe, or 500 Internal
// Server Error listing the failing plugins.
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
			_, _ = fmt.Fprintf(resp, "probe failed for device plugin %q\n", name)
		}
	}
}

// Add creates a new plugin for given Resource and registers it with the
// kubelet. Attempts to register resource with the same name twice will result
// in an error.
func (r *Registry) Add(resource Resource) error {
	plugin, err := newPlugin(resource, r.ctx, r.wg, r.pluginDir, r.reregister)
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
