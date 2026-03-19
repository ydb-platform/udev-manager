package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/ydb-platform/udev-manager/internal/plugin"
	"github.com/ydb-platform/udev-manager/internal/udev"
)

func makePartitionDevice(id, devNode, partName string) *udev.FakeDevice {
	return udev.NewFakeDevice(udev.Id(id)).
		WithSubsystem("block").
		WithDevType("partition").
		WithDevNode(devNode).
		WithProperty("PARTNAME", partName).
		WithSysAttr("wwid", "naa.50000"+partName).
		WithSysAttr("model", "TestNVMe").
		WithSysAttr("serial", "SN-"+partName)
}

func makeNetDevice(id, ifname string, speedMbps int, operstate string) *udev.FakeDevice {
	return udev.NewFakeDevice(udev.Id(id)).
		WithSubsystem("net").
		WithProperty("INTERFACE", ifname).
		WithSysAttr("speed", fmt.Sprintf("%d", speedMbps)).
		WithSysAttr("operstate", operstate)
}

// waitForRegistrations waits until the fake kubelet has received n registrations.
func waitForRegistrations(kubelet *fakeKubelet, n int) {
	EventuallyWithOffset(1, func() []*pluginapi.RegisterRequest {
		return kubelet.Registrations()
	}, 5*time.Second, 50*time.Millisecond).Should(HaveLen(n))
}

// waitForSockets waits until at least one plugin socket appears in dir and returns them.
func waitForSockets(dir string) []string {
	var sockets []string
	EventuallyWithOffset(1, func() []string {
		sockets = findPluginSockets(dir)
		return sockets
	}, 5*time.Second, 50*time.Millisecond).ShouldNot(BeEmpty())
	return sockets
}

// startTestApp is a helper that calls startApp with test options and registers cleanup.
func startTestApp(
	ctx context.Context, wg *sync.WaitGroup,
	discovery *udev.FakeDiscovery, config *appConfig,
	tmpDir, kubeSock string,
) *plugin.Registry {
	registry, cleanup, err := startApp(ctx, wg, discovery, config,
		plugin.WithPluginDir(tmpDir+"/"),
		plugin.WithKubeletSocket(kubeSock),
	)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(cleanup)
	return registry
}

var _ = Describe("E2E", func() {
	var (
		ctx       context.Context
		cancel    context.CancelFunc
		wg        *sync.WaitGroup
		tmpDir    string
		discovery *udev.FakeDiscovery
		kubelet   *fakeKubelet
		kubeSock  string
	)

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "dp")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { os.RemoveAll(tmpDir) })

		ctx, cancel = context.WithCancel(context.Background())

		wg = &sync.WaitGroup{}
		DeferCleanup(func() {
			cancel()
			wg.Wait()
		})

		discovery = udev.NewFakeDiscovery()
		DeferCleanup(discovery.Close)

		kubelet, kubeSock = startFakeKubelet(tmpDir)
		DeferCleanup(kubelet.Stop)
	})

	Describe("Partition device lifecycle", func() {
		It("registers, streams devices, and allocates", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)

			By("waiting for plugin socket and registration")
			sockets := waitForSockets(tmpDir)
			waitForRegistrations(kubelet, 1)

			reg := kubelet.Registrations()[0]
			Expect(reg.ResourceName).To(Equal("ydb.tech/part-disk01"))
			Expect(reg.Version).To(Equal(pluginapi.Version))

			By("connecting to the plugin and calling ListAndWatch")
			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp := recvWithTimeout(stream, 5*time.Second)
			Expect(resp.Devices).To(HaveLen(1))
			Expect(resp.Devices[0].ID).To(Equal("disk01"))
			Expect(resp.Devices[0].Health).To(Equal("Healthy"))

			By("adding a second device dynamically (different label = new resource)")
			dev2 := makePartitionDevice("/sys/block/nvme0n2/nvme0n2p1", "/dev/nvme0n2p1", "nvme_disk02")
			discovery.Emit(udev.Added{Device: dev2})
			waitForRegistrations(kubelet, 2)

			By("removing the first device marks it unhealthy")
			discovery.Emit(udev.Removed{Device: dev})

			unhealthyResp := drainUntil(stream, 5*time.Second, func(r *pluginapi.ListAndWatchResponse) bool {
				for _, d := range r.Devices {
					if d.Health == "Unhealthy" {
						return true
					}
				}
				return false
			})
			Expect(unhealthyResp.Devices).To(HaveLen(1))
			Expect(unhealthyResp.Devices[0].Health).To(Equal("Unhealthy"))

			By("allocating a device returns correct DeviceSpec and env vars")
			allocResp, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
				ContainerRequests: []*pluginapi.ContainerAllocateRequest{
					{DevicesIDs: []string{"disk01"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(allocResp.ContainerResponses).To(HaveLen(1))

			cr := allocResp.ContainerResponses[0]
			Expect(cr.Devices).To(HaveLen(1))
			Expect(cr.Devices[0].HostPath).To(Equal("/dev/nvme0n1p1"))
			Expect(cr.Devices[0].ContainerPath).To(Equal("/dev/allocated/ydb.tech/part/disk01"))
			Expect(cr.Devices[0].Permissions).To(Equal("rw"))

			Expect(cr.Envs).To(HaveKeyWithValue("YDB_TECH_PART_DISK01_PATH", "/dev/allocated/ydb.tech/part/disk01"))
			Expect(cr.Envs).To(HaveKeyWithValue("YDB_TECH_PART_DISK01_DISK_ID", "naa.50000nvme_disk01"))
			Expect(cr.Envs).To(HaveKeyWithValue("YDB_TECH_PART_DISK01_DISK_MODEL", "TestNVMe"))
			Expect(cr.Envs).To(HaveKeyWithValue("YDB_TECH_PART_DISK01_DISK_SERIAL", "SN-nvme_disk01"))
		})
	})

	Describe("Partition with topology hints", func() {
		It("includes NUMA node in ListAndWatch when device has one", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01").
				WithNumaNode(1)
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)
			sockets := waitForSockets(tmpDir)

			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp := recvWithTimeout(stream, 5*time.Second)
			Expect(resp.Devices).To(HaveLen(1))
			Expect(resp.Devices[0].Topology).NotTo(BeNil())
			Expect(resp.Devices[0].Topology.Nodes).To(HaveLen(1))
			Expect(resp.Devices[0].Topology.Nodes[0].ID).To(BeEquivalentTo(1))
		})

		It("omits topology when disable_topology_hints is true", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01").
				WithNumaNode(1)
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
disable_topology_hints: true
partitions:
  - matcher: "nvme_(.*)"
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)
			sockets := waitForSockets(tmpDir)

			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp := recvWithTimeout(stream, 5*time.Second)
			Expect(resp.Devices).To(HaveLen(1))
			Expect(resp.Devices[0].Topology).To(BeNil())
		})
	})

	Describe("Partition with domain override", func() {
		It("uses the overridden domain in resource name and allocation", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
    domain: storage.example.com
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)
			waitForRegistrations(kubelet, 1)

			reg := kubelet.Registrations()[0]
			Expect(reg.ResourceName).To(Equal("storage.example.com/part-disk01"))

			sockets := waitForSockets(tmpDir)
			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			allocResp, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
				ContainerRequests: []*pluginapi.ContainerAllocateRequest{
					{DevicesIDs: []string{"disk01"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())

			cr := allocResp.ContainerResponses[0]
			Expect(cr.Devices[0].ContainerPath).To(Equal("/dev/allocated/storage.example.com/part/disk01"))
			Expect(cr.Envs).To(HaveKey("STORAGE_EXAMPLE_COM_PART_DISK01_PATH"))
		})
	})

	Describe("Batch partition", func() {
		It("aggregates multiple partitions into one resource with seats", func() {
			dev1 := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_data_01")
			dev2 := makePartitionDevice("/sys/block/nvme0n2/nvme0n2p1", "/dev/nvme0n2p1", "nvme_data_02")
			discovery.AddDevice(dev1)
			discovery.AddDevice(dev2)

			config := mustParseYAML(`
domain: ydb.tech
batchPartitions:
  - name: nvme-set
    matcher: "nvme_data_.*"
    count: 2
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)

			By("waiting for exactly one registration (batch = single resource)")
			waitForRegistrations(kubelet, 1)

			reg := kubelet.Registrations()[0]
			Expect(reg.ResourceName).To(Equal("ydb.tech/batch-nvme-set"))

			By("connecting and verifying 2 healthy seats")
			sockets := waitForSockets(tmpDir)

			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp := recvWithTimeout(stream, 5*time.Second)
			Expect(resp.Devices).To(HaveLen(2))
			for _, d := range resp.Devices {
				Expect(d.Health).To(Equal("Healthy"))
			}

			By("allocating a seat returns DeviceSpecs for ALL partitions in the pool")
			allocResp, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
				ContainerRequests: []*pluginapi.ContainerAllocateRequest{
					{DevicesIDs: []string{"0"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(allocResp.ContainerResponses).To(HaveLen(1))

			batchCR := allocResp.ContainerResponses[0]
			Expect(batchCR.Devices).To(HaveLen(2))

			hostPaths := []string{batchCR.Devices[0].HostPath, batchCR.Devices[1].HostPath}
			Expect(hostPaths).To(ContainElements("/dev/nvme0n1p1", "/dev/nvme0n2p1"))

			containerPaths := []string{batchCR.Devices[0].ContainerPath, batchCR.Devices[1].ContainerPath}
			Expect(containerPaths).To(ContainElements(
				"/dev/allocated/ydb.tech/part/nvme_data_01",
				"/dev/allocated/ydb.tech/part/nvme_data_02",
			))

			for _, d := range batchCR.Devices {
				Expect(d.Permissions).To(Equal("rw"))
			}

			Expect(batchCR.Envs).To(HaveKeyWithValue(
				"YDB_TECH_PART_NVME_DATA_01_PATH",
				"/dev/allocated/ydb.tech/part/nvme_data_01",
			))
			Expect(batchCR.Envs).To(HaveKeyWithValue(
				"YDB_TECH_PART_NVME_DATA_02_PATH",
				"/dev/allocated/ydb.tech/part/nvme_data_02",
			))
			Expect(batchCR.Envs).To(HaveKey("YDB_TECH_PART_NVME_DATA_01_DISK_ID"))
			Expect(batchCR.Envs).To(HaveKey("YDB_TECH_PART_NVME_DATA_01_DISK_MODEL"))
			Expect(batchCR.Envs).To(HaveKey("YDB_TECH_PART_NVME_DATA_02_DISK_ID"))
			Expect(batchCR.Envs).To(HaveKey("YDB_TECH_PART_NVME_DATA_02_DISK_MODEL"))
		})

		It("stays healthy after partial removal and goes unhealthy only when empty", func() {
			dev1 := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_data_01")
			dev2 := makePartitionDevice("/sys/block/nvme0n2/nvme0n2p1", "/dev/nvme0n2p1", "nvme_data_02")
			discovery.AddDevice(dev1)
			discovery.AddDevice(dev2)

			config := mustParseYAML(`
domain: ydb.tech
batchPartitions:
  - name: nvme-set
    matcher: "nvme_data_.*"
    count: 1
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)
			sockets := waitForSockets(tmpDir)

			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp := recvWithTimeout(stream, 5*time.Second)
			Expect(resp.Devices).To(HaveLen(1))
			Expect(resp.Devices[0].Health).To(Equal("Healthy"))

			By("removing one device — pool still non-empty, allocate returns remaining device")
			discovery.Emit(udev.Removed{Device: dev1})

			// Pool is still non-empty (dev2 remains), so no health change is
			// emitted. Verify allocate only returns the remaining device.
			allocResp, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
				ContainerRequests: []*pluginapi.ContainerAllocateRequest{
					{DevicesIDs: []string{"0"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(allocResp.ContainerResponses[0].Devices).To(HaveLen(1))
			Expect(allocResp.ContainerResponses[0].Devices[0].HostPath).To(Equal("/dev/nvme0n2p1"))

			By("removing all remaining devices makes seats unhealthy")
			discovery.Emit(udev.Removed{Device: dev2})

			unhealthyResp := drainUntil(stream, 5*time.Second, func(r *pluginapi.ListAndWatchResponse) bool {
				for _, d := range r.Devices {
					if d.Health == "Unhealthy" {
						return true
					}
				}
				return false
			})
			for _, d := range unhealthyResp.Devices {
				Expect(d.Health).To(Equal("Unhealthy"))
			}
		})
	})

	Describe("Network bandwidth", func() {
		It("creates shares based on speed/mbpsPerShare and returns empty allocate response", func() {
			dev := makeNetDevice("/sys/class/net/eth0", "eth0", 10000, "up")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
networkBandwidth:
  - matcher: "eth(.*)"
    mbpsPerShare: 1000
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)

			waitForRegistrations(kubelet, 1)
			reg := kubelet.Registrations()[0]
			Expect(reg.ResourceName).To(Equal("ydb.tech/netbw-0"))

			sockets := waitForSockets(tmpDir)
			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp := recvWithTimeout(stream, 5*time.Second)
			// 10000 / 1000 = 10 shares
			Expect(resp.Devices).To(HaveLen(10))
			for _, d := range resp.Devices {
				Expect(d.Health).To(Equal("Healthy"))
			}

			By("allocate returns empty response (no devices, mounts, or envs)")
			allocResp, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
				ContainerRequests: []*pluginapi.ContainerAllocateRequest{
					{DevicesIDs: []string{"eth0_0"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(allocResp.ContainerResponses).To(HaveLen(1))
			Expect(allocResp.ContainerResponses[0].Devices).To(BeEmpty())
			Expect(allocResp.ContainerResponses[0].Mounts).To(BeEmpty())
			Expect(allocResp.ContainerResponses[0].Envs).To(BeEmpty())
		})

		It("reports unhealthy when interface operstate is not up", func() {
			dev := makeNetDevice("/sys/class/net/eth0", "eth0", 10000, "down")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
networkBandwidth:
  - matcher: "eth(.*)"
    mbpsPerShare: 10000
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)
			sockets := waitForSockets(tmpDir)

			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp := recvWithTimeout(stream, 5*time.Second)
			Expect(resp.Devices).To(HaveLen(1))
			Expect(resp.Devices[0].Health).To(Equal("Unhealthy"))
		})
	})

	Describe("Multiple resource types", func() {
		It("registers independent resources from one config", func() {
			partDev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			batchDev := makePartitionDevice("/sys/block/sda/sda1", "/dev/sda1", "ssd_data_01")
			discovery.AddDevice(partDev)
			discovery.AddDevice(batchDev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
batchPartitions:
  - name: ssd-batch
    matcher: "ssd_.*"
    count: 1
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)

			By("verifying 2 registrations (one partition + one batch)")
			waitForRegistrations(kubelet, 2)

			names := make([]string, 2)
			for i, r := range kubelet.Registrations() {
				names[i] = r.ResourceName
			}
			Expect(names).To(ContainElements("ydb.tech/part-disk01", "ydb.tech/batch-ssd-batch"))
		})
	})

	Describe("No matching devices", func() {
		It("does not register until a matching device appears", func() {
			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)

			By("no registrations with empty discovery")
			Consistently(func() []*pluginapi.RegisterRequest {
				return kubelet.Registrations()
			}, 200*time.Millisecond, 50*time.Millisecond).Should(BeEmpty())

			By("emitting a matching device triggers registration")
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			discovery.Emit(udev.Added{Device: dev})

			waitForRegistrations(kubelet, 1)
			sockets := waitForSockets(tmpDir)

			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp := recvWithTimeout(stream, 5*time.Second)
			Expect(resp.Devices).To(HaveLen(1))
			Expect(resp.Devices[0].Health).To(Equal("Healthy"))
		})
	})

	Describe("Multiple containers in one AllocateRequest", func() {
		It("returns one ContainerAllocateResponse per container request", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)
			sockets := waitForSockets(tmpDir)

			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			allocResp, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
				ContainerRequests: []*pluginapi.ContainerAllocateRequest{
					{DevicesIDs: []string{"disk01"}},
					{DevicesIDs: []string{"disk01"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(allocResp.ContainerResponses).To(HaveLen(2))

			for _, cr := range allocResp.ContainerResponses {
				Expect(cr.Devices).To(HaveLen(1))
				Expect(cr.Devices[0].HostPath).To(Equal("/dev/nvme0n1p1"))
			}
		})
	})

	Describe("Healthz endpoint", func() {
		It("returns 200 when all plugins are healthy", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
`)

			registry := startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)
			waitForRegistrations(kubelet, 1)

			rec := httptest.NewRecorder()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/healthz", nil)
			registry.Healthz(rec, req)
			Expect(rec.Code).To(Equal(http.StatusOK))
		})

		It("returns 500 when a plugin socket is gone", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
`)

			registry := startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)
			waitForRegistrations(kubelet, 1)
			sockets := waitForSockets(tmpDir)

			By("removing the plugin socket to simulate failure")
			err := os.Remove(sockets[0])
			Expect(err).NotTo(HaveOccurred())

			rec := httptest.NewRecorder()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/healthz", nil)
			registry.Healthz(rec, req)
			Expect(rec.Code).To(Equal(http.StatusInternalServerError))
			Expect(rec.Body.String()).To(ContainSubstring("ydb.tech/part-disk01"))
		})
	})

	Describe("Allocate unknown device", func() {
		It("returns NotFound for an unknown device ID", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)
			sockets := waitForSockets(tmpDir)

			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			_, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
				ContainerRequests: []*pluginapi.ContainerAllocateRequest{
					{DevicesIDs: []string{"nonexistent"}},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})

	Describe("Kubelet restart (hup)", func() {
		It("re-registers plugins and new sockets serve ListAndWatch", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
`)

			startTestApp(ctx, wg, discovery, config, tmpDir, kubeSock)

			By("waiting for initial registration")
			waitForRegistrations(kubelet, 1)
			initialSockets := waitForSockets(tmpDir)

			By("connecting to the initial plugin socket")
			client1, conn1 := dialPlugin(initialSockets[0])
			DeferCleanup(func() { conn1.Close() })

			stream1, err := client1.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp := recvWithTimeout(stream1, 5*time.Second)
			Expect(resp.Devices).To(HaveLen(1))

			By("simulating kubelet restart: stop old kubelet, remove socket, start new one")
			kubelet.Stop()
			// GracefulStop may already have removed the socket; ignore ENOENT.
			_ = os.Remove(kubeSock)

			// Start a fresh fake kubelet on the same socket path.
			// Creating the socket triggers fsnotify CREATE → hup().
			kubelet2, _ := startFakeKubelet(tmpDir)
			DeferCleanup(kubelet2.Stop)

			By("waiting for re-registration at the new kubelet")
			Eventually(func() []*pluginapi.RegisterRequest {
				return kubelet2.Registrations()
			}, 5*time.Second, 50*time.Millisecond).Should(HaveLen(1))

			reg := kubelet2.Registrations()[0]
			Expect(reg.ResourceName).To(Equal("ydb.tech/part-disk01"))

			By("old ListAndWatch stream terminates")
			Eventually(func() error {
				_, err := stream1.Recv()
				return err
			}, 5*time.Second, 50*time.Millisecond).Should(HaveOccurred())

			By("new plugin socket serves ListAndWatch correctly")
			newSockets := waitForSockets(tmpDir)
			client2, conn2 := dialPlugin(newSockets[0])
			DeferCleanup(func() { conn2.Close() })

			stream2, err := client2.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			resp2 := recvWithTimeout(stream2, 5*time.Second)
			Expect(resp2.Devices).To(HaveLen(1))
			Expect(resp2.Devices[0].ID).To(Equal("disk01"))
			Expect(resp2.Devices[0].Health).To(Equal("Healthy"))
		})
	})

	Describe("Shutdown", func() {
		It("terminates ListAndWatch stream when context is cancelled", func() {
			dev := makePartitionDevice("/sys/block/nvme0n1/nvme0n1p1", "/dev/nvme0n1p1", "nvme_disk01")
			discovery.AddDevice(dev)

			config := mustParseYAML(`
domain: ydb.tech
partitions:
  - matcher: "nvme_(.*)"
`)

			appCtx, appCancel := context.WithCancel(ctx)
			appWg := &sync.WaitGroup{}

			registry, cleanup, err := startApp(appCtx, appWg, discovery, config,
				plugin.WithPluginDir(tmpDir+"/"),
				plugin.WithKubeletSocket(kubeSock),
			)
			Expect(err).NotTo(HaveOccurred())
			_ = registry

			sockets := waitForSockets(tmpDir)
			client, conn := dialPlugin(sockets[0])
			DeferCleanup(func() { conn.Close() })

			stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())

			// Drain the initial snapshot
			recvWithTimeout(stream, 5*time.Second)

			By("cancelling the app context shuts down the plugin")
			cleanup()
			appCancel()
			appWg.Wait()

			By("stream.Recv returns an error after shutdown")
			_, err = stream.Recv()
			Expect(err).To(HaveOccurred())
		})
	})
})
