#!/usr/bin/env bash
set -euo pipefail

STATE_DIR=${STATE_DIR:-/state}
NBD_MAX=${NBD_MAX:-8}

log() { printf 'device-lab: %s\n' "$*" >&2; }
fail() { log "$*"; return 1; }

wait_for_path() {
  local path=$1
  local attempt
  for attempt in $(seq 1 100); do
    [[ -e "${path}" ]] && return 0
    sleep 0.1
  done
  fail "timed out waiting for ${path}"
}

validate_label() {
  [[ "$1" =~ ^e2e_[A-Za-z0-9_.-]+$ ]] || fail "invalid partition label: $1"
}

validate_signature() {
  [[ "$1" =~ ^UDEV_MANAGER_E2E_[A-Z0-9_]+$ ]] || fail "invalid signature: $1"
}

validate_interface() {
  [[ "$1" =~ ^e2e[A-Za-z0-9_.-]+$ ]] || fail "invalid interface name: $1"
  (( ${#1} <= 15 )) || fail "interface name is longer than 15 characters: $1"
}

validate_rdma_name() {
  [[ "$1" =~ ^(rxe|siw)_e2e[A-Za-z0-9_.-]+$ ]] || fail "invalid RDMA device name: $1"
}

validate_nbd_base() {
  [[ "$1" =~ ^/dev/nbd([0-7])$ ]] || fail "NBD device is outside the owned test range: $1"
}

validate_nbd_partition() {
  [[ "$1" =~ ^/dev/nbd([0-7])p1$ ]] || fail "NBD partition is outside the owned test range: $1"
}

ensure_block_node() {
  local path=$1
  local name=${path##*/}
  local sysfs_dev="/sys/class/block/${name}/dev"
  local major
  local minor

  wait_for_path "${sysfs_dev}"
  if [[ ! -b "${path}" ]]; then
    IFS=: read -r major minor < "${sysfs_dev}"
    mknod "${path}" b "${major}" "${minor}"
  fi
}

ensure_char_node() {
  local path=$1
  local sysfs_dev=$2
  local major
  local minor

  wait_for_path "${sysfs_dev}"
  if [[ ! -c "${path}" ]]; then
    mkdir -p "${path%/*}"
    IFS=: read -r major minor < "${sysfs_dev}"
    mknod "${path}" c "${major}" "${minor}"
  fi
}

ensure_rdma_nodes() {
  local sysfs_dev
  if [[ -e /sys/class/infiniband_cm/rdma_cm/dev ]]; then
    ensure_char_node /dev/infiniband/rdma_cm /sys/class/infiniband_cm/rdma_cm/dev
  fi
  for sysfs_dev in /sys/class/infiniband_verbs/uverbs*/dev; do
    [[ -e "${sysfs_dev}" ]] || continue
    ensure_char_node "/dev/infiniband/$(basename "$(dirname "${sysfs_dev}")")" "${sysfs_dev}"
  done
}

select_free_nbd() {
  local index
  local device

  modprobe nbd nbds_max="${NBD_MAX}" max_part=8
  udevadm settle --timeout=30
  if [[ "$(cat /sys/module/nbd/parameters/max_part)" == 0 ]]; then
    fail "the nbd kernel driver was loaded without partition support"
  fi
  for index in $(seq 0 $((NBD_MAX - 1))); do
    device="/dev/nbd${index}"
    ensure_block_node "${device}"
    if [[ "$(cat "/sys/class/block/nbd${index}/size")" == "0" ]]; then
      printf '%s\n' "${device}"
      return 0
    fi
  done
  fail "no free NBD device in the owned test range"
}

partition_add() {
  local label=$1
  local signature=$2
  local requested=${3:-auto}
  local record
  local device
  local image
  local partition

  validate_label "${label}"
  validate_signature "${signature}"
  mkdir -p "${STATE_DIR}"
  record="${STATE_DIR}/partition-${label}.record"
  image="${STATE_DIR}/partition-${label}.img"

  if [[ -f "${record}" ]]; then
    device=$(cat "${record}")
    validate_nbd_base "${device}"
    partition="${device}p1"
    if [[ -e "/sys/class/block/${partition##*/}" ]]; then
      printf 'partition=%s\n' "${partition}"
      return 0
    fi
    fail "owned partition ${label} has stale state; run reset first"
  fi

  if [[ "${requested}" == auto ]]; then
    device=$(select_free_nbd)
  else
    device=${requested}
    validate_nbd_base "${device}"
    ensure_block_node "${device}"
    [[ "$(cat "/sys/class/block/${device##*/}/size")" == "0" ]] || fail "${device} is already connected"
  fi

  truncate --size 32M "${image}"
  printf 'label: gpt\nsize=16M, type=linux, name="%s"\n' "${label}" | sfdisk "${image}" >/dev/null
  if ! qemu-nbd --connect="${device}" --format=raw "${image}"; then
    rm -f "${image}"
    return 1
  fi
  printf '%s\n' "${device}" > "${record}"

  partition="${device}p1"
  if ! wait_for_path "/sys/class/block/${partition##*/}/dev"; then
    blockdev --rereadpt "${device}"
  fi
  ensure_block_node "${partition}"
  printf '%s\n' "${signature}" | dd of="${partition}" bs=64 count=1 conv=notrunc status=none
  udevadm settle --timeout=30
  log "created ${partition} with PARTNAME=${label}"
  printf 'partition=%s\n' "${partition}"
}

partition_expose() {
  local partition=$1
  local action=$2
  local name
  local record

  validate_nbd_partition "${partition}"
  name=${partition##*/}
  record="${STATE_DIR}/exposure-${name}.record"
  mkdir -p "${STATE_DIR}"

  case "${action}" in
    add)
      ensure_block_node "${partition}"
      printf '%s\n' "${partition}" > "${record}"
      udevadm settle --timeout=30
      ;;
    remove)
      [[ -f "${record}" ]] || return 0
      rm -f "${partition}" "${record}"
      ;;
    *) fail "partition expose action must be add or remove" ;;
  esac
}

partition_remove() {
  local label=$1
  local record
  local device

  validate_label "${label}"
  record="${STATE_DIR}/partition-${label}.record"
  [[ -f "${record}" ]] || return 0
  device=$(cat "${record}")
  validate_nbd_base "${device}"
  if [[ -e "/sys/class/block/${device##*/}" ]]; then
    ensure_block_node "${device}"
    qemu-nbd --disconnect "${device}" >/dev/null 2>&1 || true
  fi
  rm -f "${record}" "${STATE_DIR}/partition-${label}.img"
  log "disconnected ${device} owned by ${label}"
}

partition_path() {
  local label=$1
  local record
  local device

  validate_label "${label}"
  record="${STATE_DIR}/partition-${label}.record"
  [[ -f "${record}" ]] || fail "partition ${label} is not owned by this lab"
  device=$(cat "${record}")
  validate_nbd_base "${device}"
  printf '%sp1\n' "${device}"
}

partition_verify() {
  local partition=$1
  local label=$2
  local signature=$3
  local name=${partition##*/}

  validate_nbd_partition "${partition}"
  validate_label "${label}"
  validate_signature "${signature}"
  ensure_block_node "${partition}"
  udevadm info --query=property --path="/sys/class/block/${name}" | grep -Fxq 'DEVTYPE=partition'
  udevadm info --query=property --path="/sys/class/block/${name}" | grep -Fxq "PARTNAME=${label}"
  dd if="${partition}" bs=64 count=1 status=none | grep -q "${signature}"
}

net_add() {
  local interface=$1
  local peer=$2
  local record

  validate_interface "${interface}"
  validate_interface "${peer}"
  mkdir -p "${STATE_DIR}"
  record="${STATE_DIR}/net-${interface}.record"
  [[ ! -f "${record}" ]] || fail "interface ${interface} is already owned by device-lab"
  [[ ! -e "/sys/class/net/${interface}" ]] || fail "interface ${interface} already exists"

  ip link add "${interface}" type veth peer name "${peer}"
  ip link set "${peer}" up
  ip link set "${interface}" up
  wait_for_path "/sys/class/net/${interface}/speed"
  printf '%s\n' "${peer}" > "${record}"
  udevadm settle --timeout=30
  printf 'speed=%s\n' "$(cat "/sys/class/net/${interface}/speed")"
}

net_remove() {
  local interface=$1
  local record

  validate_interface "${interface}"
  record="${STATE_DIR}/net-${interface}.record"
  [[ -f "${record}" ]] || return 0
  ip link delete "${interface}" >/dev/null 2>&1 || true
  rm -f "${record}"
  udevadm settle --timeout=30 || true
}

select_rdma_driver() {
  modprobe ib_uverbs >/dev/null 2>&1 || true
  modprobe rdma_ucm >/dev/null 2>&1 || true
  if [[ -d /sys/module/rdma_rxe ]] || modprobe rdma_rxe >/dev/null 2>&1; then
    printf 'rxe\n'
    return 0
  fi
  if [[ -d /sys/module/siw ]] || modprobe siw >/dev/null 2>&1; then
    printf 'siw\n'
    return 0
  fi
  fail "neither rdma_rxe nor siw is available in kernel $(uname -r)"
}

rdma_add() {
  local interface=$1
  local peer=$2
  local rdma_device=$3
  local requested_driver=${4:-auto}
  local driver
  local record

  validate_interface "${interface}"
  validate_interface "${peer}"
  validate_rdma_name "${rdma_device}"
  record="${STATE_DIR}/rdma-${rdma_device}.record"
  [[ ! -f "${record}" ]] || fail "RDMA device ${rdma_device} is already owned by device-lab"

  if [[ "${requested_driver}" == auto ]]; then
    driver=$(select_rdma_driver)
  else
    driver=${requested_driver}
    [[ "${driver}" == rxe || "${driver}" == siw ]] || fail "RDMA driver must be auto, rxe, or siw"
  fi
  net_add "${interface}" "${peer}" >/dev/null
  if ! rdma link add "${rdma_device}" type "${driver}" netdev "${interface}"; then
    net_remove "${interface}"
    return 1
  fi
  printf '%s\n' "${interface}" > "${record}"
  wait_for_path "/sys/class/infiniband/${rdma_device}"
  ensure_rdma_nodes
  udevadm settle --timeout=30
  ibv_devices | grep -q "${rdma_device}"
  log "created ${rdma_device} with ${driver} on ${interface}"
  printf 'driver=%s\n' "${driver}"
}

rdma_remove() {
  local interface=$1
  local rdma_device=$2
  local record

  validate_interface "${interface}"
  validate_rdma_name "${rdma_device}"
  record="${STATE_DIR}/rdma-${rdma_device}.record"
  [[ -f "${record}" ]] || return 0
  ip link set "${interface}" down >/dev/null 2>&1 || true
  rdma link delete "${rdma_device}" >/dev/null 2>&1 || true
  rm -f "${record}"
  net_remove "${interface}"
}

reset() {
  local record
  local rdma_device
  local interface
  local partition
  local label

  mkdir -p "${STATE_DIR}"
  for record in "${STATE_DIR}"/rdma-*.record; do
    [[ -f "${record}" ]] || continue
    rdma_device=${record##*/rdma-}
    rdma_device=${rdma_device%.record}
    interface=$(cat "${record}")
    rdma_remove "${interface}" "${rdma_device}" || true
  done
  for record in "${STATE_DIR}"/net-*.record; do
    [[ -f "${record}" ]] || continue
    interface=${record##*/net-}
    interface=${interface%.record}
    net_remove "${interface}" || true
  done
  for record in "${STATE_DIR}"/exposure-*.record; do
    [[ -f "${record}" ]] || continue
    partition=$(cat "${record}")
    partition_expose "${partition}" remove || true
  done
  for record in "${STATE_DIR}"/partition-*.record; do
    [[ -f "${record}" ]] || continue
    label=${record##*/partition-}
    label=${label%.record}
    partition_remove "${label}" || true
  done
  udevadm settle --timeout=30 || true
  log "removed all devices owned by this lab"
}

status() {
  printf '%s\n' '--- owned state ---'
  find "${STATE_DIR}" -maxdepth 1 -type f -name '*.record' -print -exec sh -c 'printf "  "; cat "$1"' _ {} \;
  printf '%s\n' '--- block devices ---'
  lsblk -o NAME,TYPE,SIZE,PARTLABEL
  printf '%s\n' '--- e2e network devices ---'
  ip -details link show | grep -E '^[0-9]+: e2e' || true
  printf '%s\n' '--- RDMA devices ---'
  rdma link show || true
  ibv_devices || true
  ls -l /dev/infiniband 2>/dev/null || true
}

case "${1:-}" in
  partition)
    case "${2:-}" in
      add) partition_add "${3:-}" "${4:-}" "${5:-auto}" ;;
      expose) partition_expose "${3:-}" "${4:-}" ;;
      path) partition_path "${3:-}" ;;
      remove) partition_remove "${3:-}" ;;
      verify) partition_verify "${3:-}" "${4:-}" "${5:-}" ;;
      *) fail "usage: device-lab partition {add LABEL SIGNATURE [auto|DEVICE]|expose DEVICE add|remove|path LABEL|remove LABEL|verify DEVICE LABEL SIGNATURE}" ;;
    esac
    ;;
  net)
    case "${2:-}" in
      add) net_add "${3:-}" "${4:-}" ;;
      remove) net_remove "${3:-}" ;;
      *) fail "usage: device-lab net {add DEVICE PEER|remove DEVICE}" ;;
    esac
    ;;
  rdma)
    case "${2:-}" in
      add) rdma_add "${3:-}" "${4:-}" "${5:-}" "${6:-auto}" ;;
      remove) rdma_remove "${3:-}" "${4:-}" ;;
      *) fail "usage: device-lab rdma {add DEVICE PEER RDMA_DEVICE [auto|rxe|siw]|remove DEVICE RDMA_DEVICE}" ;;
    esac
    ;;
  reset) reset ;;
  status) status ;;
  *)
    printf 'usage: %s {partition ...|net ...|rdma ...|reset|status}\n' "$0" >&2
    exit 2
    ;;
esac
