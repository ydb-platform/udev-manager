package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// fakeKubelet is a minimal gRPC server that implements the kubelet
// Registration service. Tests use it to verify that the device plugin
// registry correctly registers itself.
type fakeKubelet struct {
	pluginapi.UnimplementedRegistrationServer
	mu            sync.Mutex
	registrations []*pluginapi.RegisterRequest
	server        *grpc.Server
	socketPath    string
}

func (fk *fakeKubelet) Register(_ context.Context, req *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	fk.registrations = append(fk.registrations, req)
	return &pluginapi.Empty{}, nil
}

// Registrations returns a snapshot of all received registration requests.
func (fk *fakeKubelet) Registrations() []*pluginapi.RegisterRequest {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	out := make([]*pluginapi.RegisterRequest, len(fk.registrations))
	copy(out, fk.registrations)
	return out
}

// Stop gracefully stops the fake kubelet gRPC server.
func (fk *fakeKubelet) Stop() {
	fk.server.GracefulStop()
}

// startFakeKubelet starts a fake kubelet Registration gRPC server on a Unix
// socket inside dir. Returns the fakeKubelet handle and the full socket path.
// Fails the current Ginkgo test on error.
func startFakeKubelet(dir string) (*fakeKubelet, string) {
	socketPath := filepath.Join(dir, "kubelet.sock")

	listener, err := net.Listen("unix", socketPath)
	Expect(err).NotTo(HaveOccurred(), "startFakeKubelet: listen")

	fk := &fakeKubelet{
		server:     grpc.NewServer(),
		socketPath: socketPath,
	}
	pluginapi.RegisterRegistrationServer(fk.server, fk)

	go func() {
		defer GinkgoRecover()
		if err := fk.server.Serve(listener); err != nil && err != grpc.ErrServerStopped {
			Fail(fmt.Sprintf("startFakeKubelet: serve: %v", err))
		}
	}()

	return fk, socketPath
}

// dialPlugin creates a gRPC client connection to the device plugin at
// socketPath and returns a DevicePluginClient. Fails the current Ginkgo test
// on error. The caller must close the returned connection when done.
func dialPlugin(socketPath string) (pluginapi.DevicePluginClient, *grpc.ClientConn) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	Expect(err).NotTo(HaveOccurred(), "dialPlugin")
	return pluginapi.NewDevicePluginClient(conn), conn
}

// recvWithTimeout receives a single ListAndWatch response with a deadline.
// Returns the response or fails the Ginkgo test if the timeout elapses.
func recvWithTimeout(stream pluginapi.DevicePlugin_ListAndWatchClient, timeout time.Duration) *pluginapi.ListAndWatchResponse {
	type result struct {
		resp *pluginapi.ListAndWatchResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		r, e := stream.Recv()
		ch <- result{r, e}
	}()
	select {
	case res := <-ch:
		ExpectWithOffset(1, res.err).NotTo(HaveOccurred(), "recvWithTimeout")
		return res.resp
	case <-time.After(timeout):
		Fail("recvWithTimeout: timed out waiting for ListAndWatch response")
		return nil // unreachable
	}
}

// drainUntil receives from the ListAndWatch stream until predicate returns
// true for a response, or the timeout elapses. This avoids blocking inside
// Eventually loops.
func drainUntil(stream pluginapi.DevicePlugin_ListAndWatchClient, timeout time.Duration, pred func(*pluginapi.ListAndWatchResponse) bool) *pluginapi.ListAndWatchResponse {
	deadline := time.After(timeout)
	for {
		type result struct {
			resp *pluginapi.ListAndWatchResponse
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			r, e := stream.Recv()
			ch <- result{r, e}
		}()
		select {
		case res := <-ch:
			ExpectWithOffset(1, res.err).NotTo(HaveOccurred(), "drainUntil: recv")
			if pred(res.resp) {
				return res.resp
			}
		case <-deadline:
			Fail("drainUntil: timed out waiting for matching response")
			return nil
		}
	}
}

// findPluginSockets returns all .sock files in dir (excluding kubelet.sock).
func findPluginSockets(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var sockets []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".sock" && e.Name() != "kubelet.sock" {
			sockets = append(sockets, filepath.Join(dir, e.Name()))
		}
	}
	return sockets
}
