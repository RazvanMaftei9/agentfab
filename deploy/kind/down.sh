#!/usr/bin/env bash
# Tear down the agentfab kind test cluster. Idempotent.

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-agentfab-test}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOST_STATE_DIR="${HOST_STATE_DIR:-${SCRIPT_DIR}/state/${CLUSTER_NAME}}"

if kind get clusters | grep -qx "${CLUSTER_NAME}"; then
    echo "==> Deleting kind cluster '${CLUSTER_NAME}'"
    kind delete cluster --name "${CLUSTER_NAME}"
else
    echo "kind cluster '${CLUSTER_NAME}' not present, nothing to delete"
fi

if [[ -d "${HOST_STATE_DIR}" ]]; then
    echo "==> Removing kind state directory '${HOST_STATE_DIR}'"
    rm -rf "${HOST_STATE_DIR}"
fi
