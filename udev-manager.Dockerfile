FROM golang:1.24-bookworm as build

WORKDIR /go/app
COPY . /go/app

ARG GOARCH=amd64

RUN apt-get update && \
    apt-get install -y --no-install-recommends libudev-dev && \
    rm -rf /var/lib/apt/lists/*
RUN CGO_ENABLED=1 GOOS=linux GOARCH=${GOARCH} go build -o udev-manager ./cmd/udev-manager

FROM debian:bookworm

WORKDIR /root

RUN apt update && apt install -y libudev1 && apt clean
RUN echo '#!/bin/bash\n\
set -e\n\
\n\
# Add initialization logic here if needed\n\
\n\
exec /usr/bin/udev-manager --config file:/etc/udev-manager/config.yaml ${UDEV_MANAGER_ARGS[@]}' > /usr/local/bin/entrypoint.sh && \
    chmod +x /usr/local/bin/entrypoint.sh
COPY --from=build /go/app/udev-manager /usr/bin/udev-manager

ENV UDEV_MANAGER_ARGS=
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
