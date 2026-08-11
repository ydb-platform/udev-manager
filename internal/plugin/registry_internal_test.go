package plugin

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testRegistry(t *testing.T) (*Registry, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	dir := t.TempDir()
	return &Registry{
		ctx:           ctx,
		wg:            wg,
		pluginDir:     dir + "/",
		kubeletSocket: filepath.Join(dir, "missing-kubelet.sock"),
	}, cancel, wg
}

func testResource(name string) *resource {
	parts := strings.SplitN(name, "/", 2)
	return newResource(
		ResourceTemplate{Domain: parts[0], Prefix: parts[1]},
		make(map[Id]Instance),
	)
}

func waitForWaitGroup(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("wait group did not finish within %s", timeout)
	}
}

func TestRegistryDuplicateDoesNotReplaceLiveSocket(t *testing.T) {
	registry, cancel, wg := testRegistry(t)
	first := testResource("example.com/device")
	second := testResource("example.com/device")
	defer first.Close()
	defer second.Close()
	defer func() {
		cancel()
		waitForWaitGroup(t, wg, 2*time.Second)
	}()

	if err := registry.Add(first); err != nil {
		t.Fatalf("add first resource: %v", err)
	}

	loaded, ok := registry.plugins.Load(first.Name())
	if !ok {
		t.Fatal("first plugin was not stored")
	}
	firstPlugin := loaded.(*plugin)
	if err := firstPlugin.probe(context.Background()); err != nil {
		t.Fatalf("probe first plugin before duplicate: %v", err)
	}

	if err := registry.Add(second); err == nil {
		t.Fatal("adding a duplicate resource unexpectedly succeeded")
	}
	if err := firstPlugin.probe(context.Background()); err != nil {
		t.Fatalf("duplicate add disrupted the live plugin socket: %v", err)
	}
}

func TestRegistrationRetryStopsWithPlugin(t *testing.T) {
	registry, cancel, wg := testRegistry(t)
	resource := testResource("example.com/device")
	defer resource.Close()
	defer cancel()

	if err := registry.Add(resource); err != nil {
		t.Fatalf("add resource: %v", err)
	}

	loaded, ok := registry.plugins.Load(resource.Name())
	if !ok {
		t.Fatal("plugin was not stored")
	}
	loaded.(*plugin).stop()

	// The kubelet socket does not exist, so a registration RPC would normally
	// wait for its 10-second deadline. Stopping the plugin must cancel it now.
	waitForWaitGroup(t, wg, 2*time.Second)
}
