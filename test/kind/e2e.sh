#!/usr/bin/env bash
set -euo pipefail

KIND_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${KIND_DIR}/../.." && pwd)
export KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-udev-manager-e2e-$$}

# shellcheck source=kind.sh
source "${KIND_DIR}/kind.sh"

previous_context=$(kubectl config current-context 2>/dev/null || true)
cluster_owned=0

restore_context() {
  if [[ -n "${previous_context}" ]]; then
    kubectl config use-context "${previous_context}" >/dev/null 2>&1 || true
  else
    kubectl config unset current-context >/dev/null 2>&1 || true
  fi
}

cleanup() {
  local status=$?
  trap - EXIT

  if [[ ${status} -ne 0 && "${cluster_owned}" == 1 ]] && cluster_exists; then
    echo "E2E failed; collecting diagnostics" >&2
    collect_logs
  fi

  if [[ "${cluster_owned}" == 1 && "${KEEP_E2E_CLUSTER:-0}" != 1 ]]; then
    if cluster_exists; then
      if kubectl --context "${KIND_CONTEXT}" get namespace "${NAMESPACE}" >/dev/null 2>&1; then
        reset_all_devices || true
      fi
      kind delete cluster --name "${KIND_CLUSTER_NAME}" || true
    fi
  elif [[ "${cluster_owned}" == 1 ]]; then
    echo "Keeping cluster ${KIND_CLUSTER_NAME}; inspect it with kubectl --context ${KIND_CONTEXT}"
  fi
  restore_context
  exit "${status}"
}
trap cleanup EXIT

preflight
require_command go
if cluster_exists; then
  echo "refusing to reuse existing E2E cluster ${KIND_CLUSTER_NAME}" >&2
  exit 1
fi
cluster_owned=1
ensure_cluster
deploy 0

mkdir -p "${ARTIFACT_ROOT}/${KIND_CLUSTER_NAME}"
export E2E_CONTEXT=${KIND_CONTEXT}
export E2E_NAMESPACE=${NAMESPACE}
export E2E_DEVICE_LAB_IMAGE=${DEVICE_LAB_IMAGE}

(
  ginkgo_args=(-ginkgo.v "-ginkgo.junit-report=${ARTIFACT_ROOT}/${KIND_CLUSTER_NAME}/junit.xml")
  if [[ -n "${E2E_FOCUS:-}" ]]; then
    ginkgo_args+=("-ginkgo.focus=${E2E_FOCUS}")
  fi
  cd "${REPO_ROOT}/test/e2e"
  go test -v . -count=1 "${ginkgo_args[@]}"
)

echo "Kind E2E passed with real kernel devices on $(uname -s)/$(uname -m)"
