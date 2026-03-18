# udev-manager

Kubernetes device plugin that exposes udev-managed devices (disk partitions, network bandwidth, RDMA) as allocatable resources.

## Quick start

1. Build an image for deployment

```bash
docker build -f udev-manager.Dockerfile --network=host -t <tag> .
```

2. Deploy as a DaemonSet with a config file:

```yaml
domain: 'ydb.tech'
partitions:
  - matcher: 'ydb_disk_(.*)'
```

3. Matching partitions appear as allocatable resources on the node:

```
Allocatable:
  ydb.tech/part-ssd_01:  1
  ydb.tech/part-ssd_02:  1
  ydb.tech/part-ssd_03:  1
```

## Configuration

The binary accepts a `--config` flag with one of:
- `file:<path>` — read from a file
- `env:<VAR>` — read from an environment variable
- `stdin` — read from standard input

### Config reference

| Field | Type | Description |
|---|---|---|
| `domain` | string | **Required.** Resource domain (e.g. `ydb.tech`). |
| `disable_topology_hints` | bool | Disable NUMA topology hints for partition devices. |
| `health_check_port` | uint16 | Port for `/healthz` endpoint (default: `8080`). |
| `partitions` | list | Expose each matching partition as its own resource. |
| `batchPartitions` | list | Group matching partitions into a single resource. |
| `networkBandwidth` | list | Expose network bandwidth shares as resources. |
| `networkRdma` | list | Expose RDMA device resources. |

### Partitions

Each partition matching the regexp becomes its own Kubernetes resource named `{domain}/part-{label}`, where `{label}` comes from the first capture group.

```yaml
partitions:
  - matcher: 'ydb_disk_(.*)'
  - matcher: 'ssd_(.*)'
    domain: storage.example.com  # optional domain override
```

### Batch partitions

All partitions matching the regexp are grouped into a single resource. A pod requesting one unit receives device specs and env vars for every matching partition at once. This is useful when a workload must own a full set of disks (e.g. a striped volume).

```yaml
batchPartitions:
  - name: nvme-data          # resource: {domain}/batch-nvme-data
    matcher: 'nvme_data_.*'
    count: 2                  # up to 2 pods can hold this simultaneously
  - name: ssd-wal
    matcher: 'ssd_wal_.*'    # count defaults to 1 (exclusive access)
```

### Network bandwidth

Exposes bandwidth shares for a network interface. Each share represents `mbpsPerShare` Mbps.

```yaml
networkBandwidth:
  - matcher: '(^eth0$)'
    mbpsPerShare: 1000
```

### Network RDMA

Exposes RDMA character devices for a network interface.

```yaml
networkRdma:
  - matcher: '(^ib0$)'
    resourceCount: 4
```

## Development

Requires Docker for building and testing (the project depends on `libudev`, which is Linux-only).

```bash
make build   # compile
make test    # run tests with -race
```

## Examples

See the [examples/](examples/) directory for sample configurations.
