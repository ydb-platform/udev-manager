package plugin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Registry is a lifecycle manager for plugins.
// It is responsible for (re-)registering plugins with the kubelet.
type Registry struct {
	plugins       sync.Map
	ctx           context.Context
	wg            *sync.WaitGroup
	watcher       *fsnotify.Watcher
	pluginDir     string
	kubeletSocket string
	kubeletInfo   os.FileInfo
	initOnce      sync.Once
	initMu        sync.RWMutex
	initialized   bool
	initErr       error
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

// hup registers all plugins with the freshly kubelet.
// Newely started kubelet removes all socket files, so we need to re-register
// all plugins. See https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/#handling-kubelet-restarts
func (r *Registry) hup() {
	r.plugins.Range(func(key, p interface{}) bool {
		old := p.(*plugin)
		old.stop()
		newP, err := newPlugin(old.resource, r.ctx, r.wg, r.pluginDir)
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
	// Remember the socket that was present when the watcher started. Some
	// fsnotify backends can emit duplicate Create events for an existing Unix
	// socket; file identity lets us distinguish those from a kubelet restart.
	registry.kubeletInfo, _ = os.Stat(registry.kubeletSocket)

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
				if event.Name == r.kubeletSocket {
					if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
						r.kubeletInfo = nil
					}
					if event.Op&fsnotify.Create != 0 && r.kubeletSocketChanged() {
						r.hup()
					}
				}
			case err, ok := <-r.watcher.Errors:
				if ok {
					klog.Errorf("kubelet socket watcher error: %v", err)
				}
			case <-r.ctx.Done():
				// Parent context is done, exit the goroutine.
				return
			}
		}
	}(registry)

	return registry, nil
}

func (r *Registry) kubeletSocketChanged() bool {
	info, err := os.Stat(r.kubeletSocket)
	if err != nil {
		klog.Errorf("failed to stat kubelet socket %q after create event: %v", r.kubeletSocket, err)
		return false
	}
	if r.kubeletInfo != nil && os.SameFile(r.kubeletInfo, info) {
		return false
	}
	r.kubeletInfo = info
	return true
}

// SetInitialized records the result of initial udev discovery and resource
// registration. The first result wins because discovery has exactly one Init
// event. Readyz remains false when initialization reports any error.
func (r *Registry) SetInitialized(err error) {
	r.initOnce.Do(func() {
		r.initMu.Lock()
		r.initialized = true
		r.initErr = err
		r.initMu.Unlock()

		if err != nil {
			klog.Errorf("initial device discovery or registration failed: %v", err)
		} else {
			klog.Info("initial device discovery and registration completed")
		}
	})
}

// Healthz is a constant-time liveness handler. It deliberately does not dial
// every plugin socket: liveness must remain responsive while initial discovery
// is CPU-intensive and as the number of device resources grows.
func (r *Registry) Healthz(resp http.ResponseWriter, _ *http.Request) {
	select {
	case <-r.ctx.Done():
		http.Error(resp, "shutting down", http.StatusServiceUnavailable)
	default:
		resp.WriteHeader(http.StatusOK)
	}
}

// Readyz reports whether the complete initial udev snapshot was processed and
// every staged resource registration succeeded. It is also suitable for a
// Kubernetes startup probe.
func (r *Registry) Readyz(resp http.ResponseWriter, _ *http.Request) {
	select {
	case <-r.ctx.Done():
		http.Error(resp, "shutting down", http.StatusServiceUnavailable)
		return
	default:
	}

	r.initMu.RLock()
	initialized := r.initialized
	initErr := r.initErr
	r.initMu.RUnlock()

	if !initialized {
		http.Error(resp, "initial device discovery is still in progress", http.StatusServiceUnavailable)
		return
	}
	if initErr != nil {
		http.Error(resp, fmt.Sprintf("initial device discovery failed: %v", initErr), http.StatusServiceUnavailable)
		return
	}
	resp.WriteHeader(http.StatusOK)
}

// Add creates a new plugin for given Resource and registers it with the
// kubelet. Attempts to register resource with the same name twice will result
// in an error.
func (r *Registry) Add(resource Resource) error {
	plugin, err := newPlugin(resource, r.ctx, r.wg, r.pluginDir)
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
