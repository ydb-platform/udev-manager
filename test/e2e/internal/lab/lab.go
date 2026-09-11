package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultNamespace = "udev-manager-e2e"
	defaultImage     = "udev-manager-device-lab:kind-e2e"
	commandTimeout   = 3 * time.Minute
	resourceTimeout  = 2 * time.Minute
)

type Lab struct {
	context   string
	namespace string
	image     string
}

type Topology struct {
	ControlPlanes []string
	Workers       []string
}

func (t Topology) All() []string {
	result := make([]string, 0, len(t.ControlPlanes)+len(t.Workers))
	result = append(result, t.ControlPlanes...)
	result = append(result, t.Workers...)
	return result
}

func (t Topology) Without(excluded string) []string {
	var result []string
	for _, node := range t.All() {
		if node != excluded {
			result = append(result, node)
		}
	}
	return result
}

type Partition struct {
	Label     string
	Signature string
	Device    string
	Owner     string
}

type Consumer struct {
	Name        string
	Resource    string
	Command     string
	HostNetwork bool
}

func NewFromEnv() (*Lab, error) {
	contextName := os.Getenv("E2E_CONTEXT")
	if contextName == "" {
		return nil, errors.New("E2E_CONTEXT is required")
	}

	namespace := os.Getenv("E2E_NAMESPACE")
	if namespace == "" {
		namespace = defaultNamespace
	}
	image := os.Getenv("E2E_DEVICE_LAB_IMAGE")
	if image == "" {
		image = defaultImage
	}

	return &Lab{context: contextName, namespace: namespace, image: image}, nil
}

func (l *Lab) kubectl(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	commandArgs := append([]string{"--context", l.context}, args...)
	command := exec.CommandContext(commandCtx, "kubectl", commandArgs...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w\n%s", strings.Join(commandArgs, " "), err, output)
	}
	return output, nil
}

func (l *Lab) Topology(ctx context.Context) (Topology, error) {
	output, err := l.kubectl(ctx, nil, "get", "nodes", "-o", "json")
	if err != nil {
		return Topology{}, err
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(output, &list); err != nil {
		return Topology{}, fmt.Errorf("decode nodes: %w", err)
	}

	var result Topology
	for _, node := range list.Items {
		_, controlPlane := node.Metadata.Labels["node-role.kubernetes.io/control-plane"]
		_, legacyMaster := node.Metadata.Labels["node-role.kubernetes.io/master"]
		if controlPlane || legacyMaster {
			result.ControlPlanes = append(result.ControlPlanes, node.Metadata.Name)
		} else {
			result.Workers = append(result.Workers, node.Metadata.Name)
		}
	}
	sort.Strings(result.ControlPlanes)
	sort.Strings(result.Workers)
	return result, nil
}

func (l *Lab) podOnNode(ctx context.Context, app, node string) (string, bool, error) {
	output, err := l.kubectl(ctx, nil, "get", "pods", "--namespace", l.namespace,
		"--selector", "app="+app, "--field-selector", "spec.nodeName="+node, "-o", "json")
	if err != nil {
		return "", false, err
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(output, &list); err != nil {
		return "", false, fmt.Errorf("decode %s pods on %s: %w", app, node, err)
	}
	for _, pod := range list.Items {
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
				break
			}
		}
		if pod.Status.Phase == "Running" {
			return pod.Metadata.Name, ready, nil
		}
	}
	return "", false, nil
}

func (l *Lab) WaitDaemonSetOnNode(ctx context.Context, app, node string) error {
	deadline := time.Now().Add(resourceTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_, ready, err := l.podOnNode(ctx, app, node)
		if err == nil && ready {
			return nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("pod app=%s on %s did not become ready: %w", app, node, lastErr)
}

func (l *Lab) execInPod(ctx context.Context, app, node, container string, args ...string) ([]byte, error) {
	pod, _, err := l.podOnNode(ctx, app, node)
	if err != nil {
		return nil, err
	}
	if pod == "" {
		return nil, fmt.Errorf("no app=%s pod is running on %s", app, node)
	}

	kubectlArgs := []string{"exec", "--namespace", l.namespace, pod, "--container", container, "--"}
	kubectlArgs = append(kubectlArgs, args...)
	return l.kubectl(ctx, nil, kubectlArgs...)
}

func (l *Lab) deviceLab(ctx context.Context, node string, args ...string) ([]byte, error) {
	command := append([]string{"/usr/local/bin/device-lab"}, args...)
	return l.execInPod(ctx, "device-lab", node, "device-lab", command...)
}

func (l *Lab) VerifyManagerExecutable(ctx context.Context, node string) error {
	_, err := l.execInPod(ctx, "udev-manager", node, "udev-manager", "/bin/sh", "-ec",
		`test "$(readlink /proc/1/exe)" = /usr/bin/udev-manager`)
	return err
}

func (l *Lab) ResetAll(ctx context.Context) error {
	topology, err := l.Topology(ctx)
	if err != nil {
		return err
	}
	var result error
	for _, node := range topology.All() {
		if _, err := l.deviceLab(ctx, node, "reset"); err != nil {
			result = errors.Join(result, fmt.Errorf("reset %s: %w", node, err))
		}
	}
	return result
}

func (l *Lab) DeleteTestJobs(ctx context.Context) error {
	_, err := l.kubectl(ctx, nil, "delete", "jobs", "--namespace", l.namespace,
		"--selector", "app.kubernetes.io/part-of=udev-manager-e2e", "--ignore-not-found", "--wait=true")
	return err
}

func (l *Lab) AddSharedPartition(ctx context.Context, label, signature string) (Partition, error) {
	topology, err := l.Topology(ctx)
	if err != nil {
		return Partition{}, err
	}
	if len(topology.Workers) == 0 {
		return Partition{}, errors.New("a worker node is required to own shared NBD devices")
	}

	partition := Partition{Label: label, Signature: signature, Owner: topology.Workers[0]}
	output, err := l.deviceLab(ctx, partition.Owner, "partition", "add", label, signature, "auto")
	if err != nil {
		return Partition{}, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if value, ok := strings.CutPrefix(line, "partition="); ok {
			partition.Device = strings.TrimSpace(value)
		}
	}
	if partition.Device == "" {
		return Partition{}, fmt.Errorf("device-lab did not report a partition path:\n%s", output)
	}

	for _, node := range topology.All() {
		if _, err := l.deviceLab(ctx, node, "partition", "expose", partition.Device, "add"); err != nil {
			_ = l.RemoveSharedPartition(ctx, partition)
			return Partition{}, fmt.Errorf("expose %s on %s: %w", partition.Device, node, err)
		}
	}
	return partition, nil
}

func (l *Lab) RemoveSharedPartition(ctx context.Context, partition Partition) error {
	topology, err := l.Topology(ctx)
	if err != nil {
		return err
	}

	var result error
	for _, node := range topology.All() {
		if _, err := l.deviceLab(ctx, node, "partition", "expose", partition.Device, "remove"); err != nil {
			result = errors.Join(result, fmt.Errorf("remove exposure from %s: %w", node, err))
		}
	}
	if _, err := l.deviceLab(ctx, partition.Owner, "partition", "remove", partition.Label); err != nil {
		result = errors.Join(result, fmt.Errorf("disconnect %s: %w", partition.Label, err))
	}
	return result
}

func (l *Lab) VerifyPartition(ctx context.Context, node string, partition Partition, label, signature string) error {
	_, err := l.deviceLab(ctx, node, "partition", "verify", partition.Device, label, signature)
	return err
}

func (l *Lab) AddVeth(ctx context.Context, node, device, peer string) (int, error) {
	output, err := l.deviceLab(ctx, node, "net", "add", device, peer)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if value, ok := strings.CutPrefix(line, "speed="); ok {
			speed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return 0, fmt.Errorf("parse speed from %q: %w", line, err)
			}
			shares := speed / 1000
			if shares < 1 {
				return 0, fmt.Errorf("interface speed %d cannot provide a 1000 Mbps share", speed)
			}
			return shares, nil
		}
	}
	return 0, fmt.Errorf("device-lab did not report interface speed:\n%s", output)
}

func (l *Lab) RemoveNetwork(ctx context.Context, node, device string) error {
	_, err := l.deviceLab(ctx, node, "net", "remove", device)
	return err
}

func (l *Lab) AddRDMA(ctx context.Context, node, device, peer, rdmaDevice string) error {
	if _, err := l.deviceLab(ctx, node, "rdma", "add", device, peer, rdmaDevice, "auto"); err != nil {
		return err
	}
	if err := l.restartManagerOnNode(ctx, node); err != nil {
		_ = l.RemoveRDMA(ctx, node, device, rdmaDevice)
		return fmt.Errorf("restart manager on %s after adding %s: %w", node, rdmaDevice, err)
	}
	return nil
}

func (l *Lab) RemoveRDMA(ctx context.Context, node, device, rdmaDevice string) error {
	if _, err := l.deviceLab(ctx, node, "rdma", "remove", device, rdmaDevice); err != nil {
		return err
	}
	if err := l.restartManagerOnNode(ctx, node); err != nil {
		return fmt.Errorf("restart manager on %s after removing %s: %w", node, rdmaDevice, err)
	}
	return nil
}

func (l *Lab) restartManagerOnNode(ctx context.Context, node string) error {
	pod, _, err := l.podOnNode(ctx, "udev-manager", node)
	if err != nil {
		return err
	}
	if pod == "" {
		return fmt.Errorf("no udev-manager pod is running on %s", node)
	}
	if _, err := l.kubectl(ctx, nil, "delete", "pod", pod, "--namespace", l.namespace, "--wait=true"); err != nil {
		return err
	}
	return l.WaitDaemonSetOnNode(ctx, "udev-manager", node)
}

type nodeStatus struct {
	Status struct {
		Capacity    map[string]string `json:"capacity"`
		Allocatable map[string]string `json:"allocatable"`
	} `json:"status"`
}

func (l *Lab) resourceQuantity(ctx context.Context, node, section, resource string) (string, bool, error) {
	output, err := l.kubectl(ctx, nil, "get", "node", node, "-o", "json")
	if err != nil {
		return "", false, err
	}
	var status nodeStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return "", false, fmt.Errorf("decode node %s: %w", node, err)
	}
	var resources map[string]string
	switch section {
	case "capacity":
		resources = status.Status.Capacity
	case "allocatable":
		resources = status.Status.Allocatable
	default:
		return "", false, fmt.Errorf("unknown resource section %q", section)
	}
	value, found := resources[resource]
	return value, found, nil
}

func (l *Lab) WaitResource(ctx context.Context, node, section, resource string, expected int) error {
	deadline := time.Now().Add(resourceTimeout)
	wanted := strconv.Itoa(expected)
	last := "<absent>"
	for time.Now().Before(deadline) {
		value, found, err := l.resourceQuantity(ctx, node, section, resource)
		if err == nil {
			if found {
				last = value
			}
			if found && value == wanted {
				return nil
			}
		} else {
			last = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("%s %s on %s = %s, want %s", section, resource, node, last, wanted)
}

func (l *Lab) WaitResourceAbsent(ctx context.Context, node, resource string) error {
	deadline := time.Now().Add(10 * time.Second)
	consecutiveAbsent := 0
	for time.Now().Before(deadline) {
		value, found, err := l.resourceQuantity(ctx, node, "capacity", resource)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("capacity %s on %s unexpectedly exists with value %s", resource, node, value)
		}
		consecutiveAbsent++
		if consecutiveAbsent == 5 {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("capacity %s on %s did not remain absent", resource, node)
}

func (l *Lab) RunConsumer(ctx context.Context, consumer Consumer) (string, error) {
	name := "e2e-" + consumer.Name
	_, _ = l.kubectl(ctx, nil, "delete", "job", name, "--namespace", l.namespace,
		"--ignore-not-found", "--wait=true")

	manifest := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": l.namespace,
			"labels": map[string]string{
				"app.kubernetes.io/part-of": "udev-manager-e2e",
			},
		},
		"spec": map[string]any{
			"backoffLimit": 0,
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]string{
						"app.kubernetes.io/part-of": "udev-manager-e2e",
					},
				},
				"spec": map[string]any{
					"hostNetwork":   consumer.HostNetwork,
					"restartPolicy": "Never",
					"containers": []any{map[string]any{
						"name":            "verify",
						"image":           l.image,
						"imagePullPolicy": "Never",
						"command":         []string{"/bin/sh", "-ec", consumer.Command},
						"resources": map[string]any{
							"limits": map[string]string{consumer.Resource: "1"},
						},
					}},
				},
			},
		},
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encode consumer job: %w", err)
	}
	if _, err := l.kubectl(ctx, content, "apply", "-f", "-"); err != nil {
		return "", err
	}

	deadline := time.Now().Add(commandTimeout)
	for time.Now().Before(deadline) {
		output, err := l.kubectl(ctx, nil, "get", "job", name, "--namespace", l.namespace, "-o", "json")
		if err == nil {
			var job struct {
				Status struct {
					Succeeded int `json:"succeeded"`
					Failed    int `json:"failed"`
				} `json:"status"`
			}
			if json.Unmarshal(output, &job) == nil {
				if job.Status.Succeeded > 0 {
					return l.consumerNode(ctx, name)
				}
				if job.Status.Failed > 0 {
					logs, _ := l.kubectl(ctx, nil, "logs", "job/"+name, "--namespace", l.namespace, "--all-containers")
					return "", fmt.Errorf("consumer job %s failed:\n%s", name, logs)
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	logs, _ := l.kubectl(ctx, nil, "logs", "job/"+name, "--namespace", l.namespace, "--all-containers")
	return "", fmt.Errorf("consumer job %s did not complete:\n%s", name, logs)
}

func (l *Lab) consumerNode(ctx context.Context, job string) (string, error) {
	output, err := l.kubectl(ctx, nil, "get", "pods", "--namespace", l.namespace,
		"--selector", "job-name="+job, "-o", "json")
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(output, &list); err != nil {
		return "", fmt.Errorf("decode consumer pod: %w", err)
	}
	if len(list.Items) != 1 || list.Items[0].Spec.NodeName == "" {
		return "", fmt.Errorf("consumer job %s has no scheduled pod", job)
	}
	return list.Items[0].Spec.NodeName, nil
}
