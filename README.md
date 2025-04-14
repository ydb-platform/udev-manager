# udev-manager

k8s device plugin that allows to expose devices used by YDB - for now, just partitions.

## Usage

1. Build an image for deployment

```bash
docker build -f udev-manager.Dockerfile --network=host -t <tag>
```

2. Deploy it as daemonset using this config

```yaml
domain: 'ydb.tech'
partitions:
  - matcher: 'ydb_disk_(.*)'
```

3. After start you'll see following resources allocatable in Kubelet (given your
node had partitions labeled `ydb_disk_(.*)`)

```yaml
...
Allocatable:
...
  ydb.tech/part-ssd_01:       1
  ydb.tech/part-ssd_02:       1
  ydb.tech/part-ssd_03:       1
  ydb.tech/part-ssd_04:       1
  ydb.tech/part-ssd_05:       1
  ydb.tech/part-ssd_06:       1
```