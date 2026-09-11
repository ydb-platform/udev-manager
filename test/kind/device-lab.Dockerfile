FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        coreutils \
        fdisk \
        grep \
        ibverbs-providers \
        ibverbs-utils \
        iproute2 \
        kmod \
        qemu-utils \
        rdma-core \
        udev \
        util-linux && \
    rm -rf /var/lib/apt/lists/*

COPY --chmod=0755 test/kind/device-lab.sh /usr/local/bin/device-lab

ENTRYPOINT ["/usr/local/bin/device-lab"]
