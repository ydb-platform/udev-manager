package e2e_test

import (
	"context"
	"os"

	"github.com/ydb-platform/udev-manager/test/e2e/internal/lab"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("udev-manager on a multi-node Kind cluster", Ordered, Serial, func() {
	var (
		ctx      context.Context
		testEnv  *lab.Lab
		topology lab.Topology
	)

	BeforeAll(func() {
		if os.Getenv("E2E_CONTEXT") == "" {
			Skip("E2E_CONTEXT is set by make e2e")
		}

		ctx = context.Background()
		var err error
		testEnv, err = lab.NewFromEnv()
		Expect(err).NotTo(HaveOccurred())
		topology, err = testEnv.Topology(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	BeforeEach(func() {
		Expect(testEnv.DeleteTestJobs(ctx)).To(Succeed())
		Expect(testEnv.ResetAll(ctx)).To(Succeed())
	})

	AfterEach(func() {
		Expect(testEnv.DeleteTestJobs(ctx)).To(Succeed())
		Expect(testEnv.ResetAll(ctx)).To(Succeed())
	})

	It("runs the production manager and test infrastructure on every node", func() {
		Expect(topology.ControlPlanes).To(HaveLen(1))
		Expect(topology.Workers).To(HaveLen(2))

		for _, node := range topology.All() {
			Expect(testEnv.WaitDaemonSetOnNode(ctx, "udevd", node)).To(Succeed())
			Expect(testEnv.WaitDaemonSetOnNode(ctx, "device-lab", node)).To(Succeed())
			Expect(testEnv.WaitDaemonSetOnNode(ctx, "udev-manager", node)).To(Succeed())
			Expect(testEnv.VerifyManagerExecutable(ctx, node)).To(Succeed())
			Expect(testEnv.WaitResource(ctx, node, "allocatable", "devices.example/numa-node0", 2)).To(Succeed())
		}
	})

	It("adds, consumes, and removes a shared real partition", func() {
		partition, err := testEnv.AddSharedPartition(ctx, "e2e_disk-live", "UDEV_MANAGER_E2E_LIVE")
		Expect(err).NotTo(HaveOccurred())

		for _, node := range topology.All() {
			Expect(testEnv.VerifyPartition(ctx, node, partition, "e2e_disk-live", "UDEV_MANAGER_E2E_LIVE")).To(Succeed())
			Expect(testEnv.WaitResource(ctx, node, "allocatable", "devices.example/part-live", 1)).To(Succeed())
		}

		node, err := testEnv.RunConsumer(ctx, lab.Consumer{
			Name:     "partition-live",
			Resource: "devices.example/part-live",
			Command:  `test -b "$DEVICES_EXAMPLE_PART_LIVE_PATH" && dd if="$DEVICES_EXAMPLE_PART_LIVE_PATH" bs=64 count=1 status=none | grep -q UDEV_MANAGER_E2E_LIVE`,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(topology.All()).To(ContainElement(node))

		Expect(testEnv.RemoveSharedPartition(ctx, partition)).To(Succeed())
		for _, node := range topology.All() {
			Expect(testEnv.WaitResource(ctx, node, "allocatable", "devices.example/part-live", 0)).To(Succeed())
		}
	})

	It("updates a batch as partitions are added and removed", func() {
		for _, node := range topology.All() {
			Expect(testEnv.WaitResource(ctx, node, "allocatable", "devices.example/batch-data", 0)).To(Succeed())
		}

		partitionA, err := testEnv.AddSharedPartition(ctx, "e2e_batch_a", "UDEV_MANAGER_E2E_BATCH_A")
		Expect(err).NotTo(HaveOccurred())
		partitionB, err := testEnv.AddSharedPartition(ctx, "e2e_batch_b", "UDEV_MANAGER_E2E_BATCH_B")
		Expect(err).NotTo(HaveOccurred())

		for _, node := range topology.All() {
			Expect(testEnv.WaitResource(ctx, node, "allocatable", "devices.example/batch-data", 2)).To(Succeed())
		}

		_, err = testEnv.RunConsumer(ctx, lab.Consumer{
			Name:     "batch-two",
			Resource: "devices.example/batch-data",
			Command: `test -b "$DEVICES_EXAMPLE_PART_A_PATH" && test -b "$DEVICES_EXAMPLE_PART_B_PATH" && ` +
				`dd if="$DEVICES_EXAMPLE_PART_A_PATH" bs=64 count=1 status=none | grep -q UDEV_MANAGER_E2E_BATCH_A && ` +
				`dd if="$DEVICES_EXAMPLE_PART_B_PATH" bs=64 count=1 status=none | grep -q UDEV_MANAGER_E2E_BATCH_B`,
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(testEnv.RemoveSharedPartition(ctx, partitionA)).To(Succeed())
		for _, node := range topology.All() {
			Expect(testEnv.WaitResource(ctx, node, "allocatable", "devices.example/batch-data", 2)).To(Succeed())
		}

		_, err = testEnv.RunConsumer(ctx, lab.Consumer{
			Name:     "batch-one",
			Resource: "devices.example/batch-data",
			Command:  `test -z "${DEVICES_EXAMPLE_PART_A_PATH:-}" && test -b "$DEVICES_EXAMPLE_PART_B_PATH" && dd if="$DEVICES_EXAMPLE_PART_B_PATH" bs=64 count=1 status=none | grep -q UDEV_MANAGER_E2E_BATCH_B`,
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(testEnv.RemoveSharedPartition(ctx, partitionB)).To(Succeed())
		for _, node := range topology.All() {
			Expect(testEnv.WaitResource(ctx, node, "allocatable", "devices.example/batch-data", 0)).To(Succeed())
		}
	})

	It("keeps network bandwidth resources local to their node", func() {
		target := topology.Workers[0]
		shares, err := testEnv.AddVeth(ctx, target, "e2ebw-local", "e2ebw-peer")
		Expect(err).NotTo(HaveOccurred())
		Expect(shares).To(BeNumerically(">", 0))
		Expect(testEnv.WaitResource(ctx, target, "allocatable", "devices.example/netbw-local", shares)).To(Succeed())
		for _, node := range topology.Without(target) {
			Expect(testEnv.WaitResourceAbsent(ctx, node, "devices.example/netbw-local")).To(Succeed())
		}

		node, err := testEnv.RunConsumer(ctx, lab.Consumer{
			Name:     "bandwidth-local",
			Resource: "devices.example/netbw-local",
			Command:  "true",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(node).To(Equal(target))

		Expect(testEnv.RemoveNetwork(ctx, target, "e2ebw-local")).To(Succeed())
		Expect(testEnv.WaitResource(ctx, target, "allocatable", "devices.example/netbw-local", 0)).To(Succeed())
	})

	It("registers only RDMA-enabled matches and applies PF/VF filters case-insensitively", func() {
		target := topology.Workers[1]
		_, err := testEnv.AddVeth(ctx, target, "e2enonrdma", "e2enon-peer")
		Expect(err).NotTo(HaveOccurred())
		for _, node := range topology.All() {
			Expect(testEnv.WaitResourceAbsent(ctx, node, "devices.example/netrdma-nonrdma")).To(Succeed())
		}

		Expect(testEnv.AddRDMA(ctx, target, "e2erdma-any", "e2eany-peer", "rxe_e2eany")).To(Succeed())
		Expect(testEnv.WaitResource(ctx, target, "allocatable", "devices.example/netrdma-any", 2)).To(Succeed())
		for _, node := range topology.Without(target) {
			Expect(testEnv.WaitResourceAbsent(ctx, node, "devices.example/netrdma-any")).To(Succeed())
		}

		node, err := testEnv.RunConsumer(ctx, lab.Consumer{
			Name:        "rdma-local",
			Resource:    "devices.example/netrdma-any",
			HostNetwork: true,
			Command:     `test -n "$(find /dev/infiniband -maxdepth 1 -name 'uverbs*' -type c -print -quit)" && ibv_devices | grep -q rxe_e2eany`,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(node).To(Equal(target))

		Expect(testEnv.AddRDMA(ctx, target, "e2erdma-pf", "e2epf-peer", "rxe_e2epf")).To(Succeed())
		Expect(testEnv.AddRDMA(ctx, target, "e2erdma-vf", "e2evf-peer", "rxe_e2evf")).To(Succeed())
		for _, resource := range []string{"devices.example/netrdma-pf", "devices.example/netrdma-vf"} {
			for _, candidate := range topology.All() {
				Expect(testEnv.WaitResourceAbsent(ctx, candidate, resource)).To(Succeed())
			}
		}

		Expect(testEnv.RemoveRDMA(ctx, target, "e2erdma-any", "rxe_e2eany")).To(Succeed())
		Expect(testEnv.WaitResource(ctx, target, "allocatable", "devices.example/netrdma-any", 0)).To(Succeed())
	})
})
