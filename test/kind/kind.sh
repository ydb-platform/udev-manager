#!/usr/bin/env bash
set -euo pipefail

KIND_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${KIND_DIR}/../.." && pwd)
NAMESPACE=${E2E_NAMESPACE:-udev-manager-e2e}
UDEV_MANAGER_IMAGE=${UDEV_MANAGER_IMAGE:-udev-manager:kind-e2e}
DEVICE_LAB_IMAGE=${DEVICE_LAB_IMAGE:-udev-manager-device-lab:kind-e2e}
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-udev-manager-dev}
KIND_CONTEXT=kind-${KIND_CLUSTER_NAME}
ARTIFACT_ROOT=${ARTIFACT_ROOT:-${REPO_ROOT}/_artifacts/kind}

require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "required command not found: $1" >&2
    return 1
  }
}

configure_platform() {
  case "$(uname -s)" in
    Darwin)
      require_command orbctl
      if [[ "$(orbctl status)" != Running ]]; then
        orbctl start
      fi
      export DOCKER_CONTEXT=${DOCKER_CONTEXT:-orbstack}
      ;;
    Linux) ;; # Do nothing!
    *)
      echo "unsupported host OS: $(uname -s)" >&2
      return 1
      ;;
  esac
}

preflight() {
  configure_platform
  require_command docker
  require_command kind
  require_command kubectl
  docker info >/dev/null
  mkdir -p "${ARTIFACT_ROOT}"
}

cluster_exists() {
  local cluster
  while IFS= read -r cluster; do
    [[ "${cluster}" == "${KIND_CLUSTER_NAME}" ]] && return 0
  done < <(kind get clusters 2>/dev/null)
  return 1
}

ensure_cluster() {
  if ! cluster_exists; then
    kind create cluster \
      --name "${KIND_CLUSTER_NAME}" \
      --config "${KIND_DIR}/cluster.yaml" \
      --wait 180s
  else
    # Always merge its context into the user's active kubeconfig.
    kind export kubeconfig --name "${KIND_CLUSTER_NAME}"
  fi

  validate_cluster_topology
}

validate_cluster_topology() {
  local control_planes
  local workers

  control_planes=$(kubectl --context "${KIND_CONTEXT}" get nodes \
    --selector='node-role.kubernetes.io/control-plane' -o name | awk 'NF { count++ } END { print count + 0 }')
  workers=$(kubectl --context "${KIND_CONTEXT}" get nodes \
    --selector='!node-role.kubernetes.io/control-plane,!node-role.kubernetes.io/master' \
    -o name | awk 'NF { count++ } END { print count + 0 }')
  if [[ "${control_planes}" != 1 || "${workers}" != 2 ]]; then
    echo "cluster ${KIND_CLUSTER_NAME} has ${control_planes} control-plane and ${workers} worker nodes; expected 1 and 2" >&2
    echo "delete the incompatible development cluster with 'make kind-down' and run 'make kind-up' again" >&2
    return 1
  fi
}

apply_config() {
  kubectl --context "${KIND_CONTEXT}" create namespace "${NAMESPACE}" \
    --dry-run=client -o yaml | kubectl --context "${KIND_CONTEXT}" apply -f -

  kubectl --context "${KIND_CONTEXT}" create configmap udev-manager-config \
    --namespace "${NAMESPACE}" \
    --from-file=config.yaml="${KIND_DIR}/app-config.yaml" \
    --dry-run=client -o yaml | kubectl --context "${KIND_CONTEXT}" apply -f -
}

apply_manifest() {
  local manifest=$1
  sed \
    -e "s|image: udev-manager:kind-e2e|image: ${UDEV_MANAGER_IMAGE}|g" \
    -e "s|image: udev-manager-device-lab:kind-e2e|image: ${DEVICE_LAB_IMAGE}|g" \
    "${manifest}" | kubectl --context "${KIND_CONTEXT}" apply -f -
}

build_images() {
  docker build -f "${REPO_ROOT}/udev-manager.Dockerfile" \
    --build-arg GOARCH= -t "${UDEV_MANAGER_IMAGE}" "${REPO_ROOT}"
  docker build -f "${KIND_DIR}/device-lab.Dockerfile" \
    -t "${DEVICE_LAB_IMAGE}" "${REPO_ROOT}"

  kind load docker-image "${UDEV_MANAGER_IMAGE}" "${DEVICE_LAB_IMAGE}" \
    --name "${KIND_CLUSTER_NAME}"
}

all_nodes() {
  kubectl --context "${KIND_CONTEXT}" get nodes \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort
}

worker_nodes() {
  kubectl --context "${KIND_CONTEXT}" get nodes \
    --selector='!node-role.kubernetes.io/control-plane,!node-role.kubernetes.io/master' \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort
}

first_worker() {
  local node
  node=$(worker_nodes | head -n 1)
  [[ -n "${node}" ]] || {
    echo "the device lab requires at least one worker node" >&2
    return 1
  }
  printf '%s\n' "${node}"
}

device_lab_pod() {
  local node=$1
  kubectl --context "${KIND_CONTEXT}" get pod --namespace "${NAMESPACE}" \
    --selector app=device-lab --field-selector "spec.nodeName=${node}" \
    -o jsonpath='{.items[0].metadata.name}'
}

run_device_lab() {
  local node=$1
  shift
  local pod
  pod=$(device_lab_pod "${node}")
  [[ -n "${pod}" ]] || {
    echo "device-lab pod not found on ${node}" >&2
    return 1
  }
  kubectl --context "${KIND_CONTEXT}" exec --namespace "${NAMESPACE}" \
    "${pod}" --container device-lab -- /usr/local/bin/device-lab "$@"
}

reset_all_devices() {
  local node
  while IFS= read -r node; do
    run_device_lab "${node}" reset
  done < <(all_nodes)
}

restart_manager_on_node() {
  local node=$1
  local pod
  pod=$(kubectl --context "${KIND_CONTEXT}" get pod --namespace "${NAMESPACE}" \
    --selector app=udev-manager --field-selector "spec.nodeName=${node}" \
    -o jsonpath='{.items[0].metadata.name}')
  kubectl --context "${KIND_CONTEXT}" delete pod "${pod}" --namespace "${NAMESPACE}" --wait=true
  kubectl --context "${KIND_CONTEXT}" rollout status daemonset/udev-manager \
    --namespace "${NAMESPACE}" --timeout=180s
}

add_shared_partition() {
  local label=$1
  local signature=$2
  local owner
  local output
  local partition
  local node

  owner=$(first_worker)
  output=$(run_device_lab "${owner}" partition add "${label}" "${signature}" auto)
  partition=$(sed -n 's/^partition=//p' <<< "${output}" | tail -n 1)
  [[ -n "${partition}" ]] || {
    echo "device-lab did not report the created partition" >&2
    return 1
  }
  while IFS= read -r node; do
    run_device_lab "${node}" partition expose "${partition}" add
  done < <(all_nodes)
  printf '%s\n' "${partition}"
}

remove_shared_partition() {
  local label=$1
  local owner
  local partition
  local node

  owner=$(first_worker)
  partition=$(run_device_lab "${owner}" partition path "${label}")
  while IFS= read -r node; do
    run_device_lab "${node}" partition expose "${partition}" remove
  done < <(all_nodes)
  run_device_lab "${owner}" partition remove "${label}"
}

seed_devices() {
  local first
  local second

  first=$(worker_nodes | sed -n '1p')
  second=$(worker_nodes | sed -n '2p')
  if [[ -z "${first}" || -z "${second}" ]]; then
    echo "seeding requires two worker nodes" >&2
    return 1
  fi

  reset_all_devices
  add_shared_partition e2e_disk-one UDEV_MANAGER_E2E_ONE >/dev/null
  add_shared_partition e2e_batch_a UDEV_MANAGER_E2E_BATCH_A >/dev/null
  add_shared_partition e2e_batch_b UDEV_MANAGER_E2E_BATCH_B >/dev/null

  run_device_lab "${first}" net add e2ebw-uplink e2ebw-peer
  run_device_lab "${first}" rdma add e2erdma-any e2eany-peer rxe_e2eany auto
  run_device_lab "${second}" net add e2enonrdma e2enon-peer
  run_device_lab "${second}" rdma add e2erdma-pf e2epf-peer rxe_e2epf auto
  run_device_lab "${second}" rdma add e2erdma-vf e2evf-peer rxe_e2evf auto
}

deploy() {
  local seed=${1:-0}

  build_images
  apply_config

  apply_manifest "${KIND_DIR}/udevd.yaml"
  kubectl --context "${KIND_CONTEXT}" rollout restart daemonset/udevd \
    --namespace "${NAMESPACE}"
  kubectl --context "${KIND_CONTEXT}" rollout status daemonset/udevd \
    --namespace "${NAMESPACE}" --timeout=180s

  apply_manifest "${KIND_DIR}/device-lab.yaml"
  kubectl --context "${KIND_CONTEXT}" rollout restart daemonset/device-lab \
    --namespace "${NAMESPACE}"
  kubectl --context "${KIND_CONTEXT}" rollout status daemonset/device-lab \
    --namespace "${NAMESPACE}" --timeout=180s
  reset_all_devices
  if [[ "${seed}" == 1 ]]; then
    seed_devices
  fi

  apply_manifest "${KIND_DIR}/daemonset.yaml"
  kubectl --context "${KIND_CONTEXT}" rollout restart daemonset/udev-manager \
    --namespace "${NAMESPACE}"
  kubectl --context "${KIND_CONTEXT}" rollout status daemonset/udev-manager \
    --namespace "${NAMESPACE}" --timeout=180s
}

up() {
  preflight
  ensure_cluster
  deploy 1
  echo
  echo "Kind cluster ${KIND_CLUSTER_NAME} is ready with real kernel devices."
  echo "Context: ${KIND_CONTEXT} (stored in the active kubeconfig)"
  echo "Inspect: kubectl --context ${KIND_CONTEXT} get nodes"
  echo "Devices: make kind-device ACTION=status"
  echo "Delete: make kind-down"
}

down() {
  configure_platform
  require_command kind
  require_command kubectl
  if cluster_exists; then
    if kubectl --context "${KIND_CONTEXT}" get namespace "${NAMESPACE}" >/dev/null 2>&1; then
      reset_all_devices || true
    fi
    kind delete cluster --name "${KIND_CLUSTER_NAME}"
  fi
}

device() {
  local action=${1:-}
  local requested_node=${2:-}
  local label=${3:-}
  local signature=${4:-}
  local device_name=${5:-}
  local peer=${6:-}
  local rdma_device=${7:-}
  local node

  preflight
  node=${requested_node:-$(first_worker)}
  case "${action}" in
    seed) seed_devices ;;
    reset) reset_all_devices ;;
    status)
      if [[ -n "${requested_node}" ]]; then
        run_device_lab "${node}" status
      else
        while IFS= read -r node; do
          echo "=== ${node} ==="
          run_device_lab "${node}" status
        done < <(all_nodes)
      fi
      ;;
    add-partition) add_shared_partition "${label}" "${signature}" ;;
    remove-partition) remove_shared_partition "${label}" ;;
    add-veth) run_device_lab "${node}" net add "${device_name}" "${peer}" ;;
    remove-network) run_device_lab "${node}" net remove "${device_name}" ;;
    add-rdma)
      run_device_lab "${node}" rdma add "${device_name}" "${peer}" "${rdma_device}" auto
      restart_manager_on_node "${node}"
      ;;
    remove-rdma)
      run_device_lab "${node}" rdma remove "${device_name}" "${rdma_device}"
      restart_manager_on_node "${node}"
      ;;
    *)
      echo "ACTION must be seed, reset, status, add-partition, remove-partition, add-veth, remove-network, add-rdma, or remove-rdma" >&2
      return 2
      ;;
  esac
}

collect_logs() {
  local destination=${1:-${ARTIFACT_ROOT}/${KIND_CLUSTER_NAME}}
  local node

  mkdir -p "${destination}"
  kind export logs "${destination}" --name "${KIND_CLUSTER_NAME}" || true
  kubectl --context "${KIND_CONTEXT}" get all --all-namespaces -o wide \
    > "${destination}/kubernetes-resources.txt" 2>&1 || true
  kubectl --context "${KIND_CONTEXT}" describe nodes \
    > "${destination}/nodes.txt" 2>&1 || true
  kubectl --context "${KIND_CONTEXT}" logs --selector app=udev-manager --namespace "${NAMESPACE}" \
    --all-containers --prefix --tail=-1 > "${destination}/udev-manager.log" 2>&1 || true
  kubectl --context "${KIND_CONTEXT}" logs --selector app=udevd --namespace "${NAMESPACE}" \
    --all-containers --prefix --tail=-1 > "${destination}/udevd.log" 2>&1 || true

  while IFS= read -r node; do
    docker exec "${node}" /bin/sh -ec '
      udevadm info --export-db
      ip -details link show
      rdma link show 2>/dev/null || true
      lsblk -o NAME,TYPE,SIZE,PARTLABEL
      ls -l /dev/infiniband 2>/dev/null || true
      find /var/lib/udev-manager-e2e -maxdepth 1 -type f -print -exec cat {} \; 2>/dev/null || true
    ' > "${destination}/device-state-${node}.txt" 2>&1 || true
    run_device_lab "${node}" status > "${destination}/device-lab-${node}.txt" 2>&1 || true
  done < <(kind get nodes --name "${KIND_CLUSTER_NAME}")
}

main() {
  case "${1:-}" in
    up) up ;;
    down) down ;;
    device) shift; device "$@" ;;
    *) echo "usage: $0 {up|down|device ACTION [NODE ...]}" >&2; return 2 ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
