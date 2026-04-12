#!/usr/bin/env bash
# Bring up an end-to-end agentfab fabric in a local kind cluster.
#
# Steps:
#   1. Build the agentfab image and load it into kind.
#   2. Create a host-backed storage root and mount it into every kind node
#      so the fabric-visible storage tiers are genuinely shared across pods.
#   3. Generate a long-lived shared CA and create the agentfab-cluster-ca
#      Secret from it.
#   4. Apply the namespace, ConfigMap, etcd, LLM Secret, and control-plane
#      workload.
#   5. Wait for the control plane to be Ready, then mint a reusable node
#      enrollment token by exec'ing into it. Store the token as a Secret.
#   6. Apply the node Deployment (which mounts the token Secret) and the
#      conductor Pod.
#
# After this completes, attach to the conductor with:
#   kubectl exec -it -n agentfab pod/conductor -- \
#       agentfab run --config /etc/agentfab/agents.yaml \
#                    --data-dir /var/lib/agentfab \
#                    --external-nodes --skip-verify

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-agentfab-test}"
IMAGE_NAME="${IMAGE_NAME:-agentfab:dev}"
NAMESPACE="${NAMESPACE:-agentfab}"
CA_VALIDITY_DAYS="${CA_VALIDITY_DAYS:-365}"
HOST_STATE_DIR="${HOST_STATE_DIR:-${SCRIPT_DIR}/state/${CLUSTER_NAME}}"

LLM_ENV_FILE="${SCRIPT_DIR}/secrets/llm.env"

require() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "error: required tool '$1' not found in PATH" >&2
        exit 1
    fi
}

require kind
require kubectl
require docker
require openssl

if [[ ! -f "${LLM_ENV_FILE}" ]]; then
    cat >&2 <<EOF
error: ${LLM_ENV_FILE} is missing.

  cp ${SCRIPT_DIR}/secrets/llm.env.example ${LLM_ENV_FILE}
  # then edit and fill in at least one provider API key
EOF
    exit 1
fi

mkdir -p "${HOST_STATE_DIR}"
mkdir -p "${HOST_STATE_DIR}/fabric/shared" "${HOST_STATE_DIR}/fabric/agents" "${HOST_STATE_DIR}/etcd"

CLUSTER_CONFIG="$(mktemp)"
trap 'rm -rf "${CA_DIR:-}" "${CLUSTER_CONFIG}"' EXIT
sed "s|__HOST_ROOT__|${HOST_STATE_DIR}|g" "${SCRIPT_DIR}/cluster.yaml" > "${CLUSTER_CONFIG}"

cluster_has_required_mount() {
    docker inspect "${CLUSTER_NAME}-control-plane" \
        --format '{{range .Mounts}}{{println .Source "|" .Destination}}{{end}}' 2>/dev/null |
        grep -Fq "${HOST_STATE_DIR} | /var/lib/agentfab-host"
}

echo "==> Ensuring kind cluster '${CLUSTER_NAME}' exists"
EXISTING_CLUSTER=false
if kind get clusters | grep -qx "${CLUSTER_NAME}"; then
    EXISTING_CLUSTER=true
    if ! cluster_has_required_mount; then
        echo "    existing cluster is missing the shared host mount; recreating"
        kind delete cluster --name "${CLUSTER_NAME}"
        kind create cluster --name "${CLUSTER_NAME}" --config "${CLUSTER_CONFIG}"
        EXISTING_CLUSTER=false
    else
        echo "    cluster already present, skipping creation"
    fi
else
    kind create cluster --name "${CLUSTER_NAME}" --config "${CLUSTER_CONFIG}"
fi

echo "==> Building ${IMAGE_NAME}"
docker build \
    -t "${IMAGE_NAME}" \
    -f "${SCRIPT_DIR}/../Dockerfile" \
    "${REPO_ROOT}"

echo "==> Loading ${IMAGE_NAME} into kind cluster '${CLUSTER_NAME}'"
kind load docker-image "${IMAGE_NAME}" --name "${CLUSTER_NAME}"

echo "==> Applying namespace"
kubectl apply -f "${SCRIPT_DIR}/manifests/00-namespace.yaml"

echo "==> Generating shared cluster CA (valid ${CA_VALIDITY_DAYS} days)"
CA_DIR="$(mktemp -d)"
trap 'rm -rf "${CA_DIR}"' EXIT
openssl ecparam -name prime256v1 -genkey -noout -out "${CA_DIR}/ca-key.pem"
openssl req -new -x509 \
    -key "${CA_DIR}/ca-key.pem" \
    -out "${CA_DIR}/ca.pem" \
    -days "${CA_VALIDITY_DAYS}" \
    -subj "/O=agentfab/CN=agentfab-cluster-ca" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" >/dev/null 2>&1

echo "==> Creating agentfab-cluster-ca Secret"
kubectl create secret generic agentfab-cluster-ca \
    --namespace "${NAMESPACE}" \
    --from-file=ca.pem="${CA_DIR}/ca.pem" \
    --from-file=ca-key.pem="${CA_DIR}/ca-key.pem" \
    --dry-run=client -o yaml | kubectl apply -f -

echo "==> Creating agentfab-llm Secret from ${LLM_ENV_FILE}"
kubectl create secret generic agentfab-llm \
    --namespace "${NAMESPACE}" \
    --from-env-file="${LLM_ENV_FILE}" \
    --dry-run=client -o yaml | kubectl apply -f -

echo "==> Applying fabric ConfigMap"
kubectl apply -f "${SCRIPT_DIR}/manifests/10-config.yaml"

echo "==> Applying etcd"
kubectl apply -f "${SCRIPT_DIR}/manifests/15-etcd.yaml"

echo "==> Waiting for etcd Deployment to become Available"
kubectl wait --namespace "${NAMESPACE}" \
    --for=condition=Available deployment/etcd \
    --timeout=180s

echo "==> Applying control plane"
kubectl apply -f "${SCRIPT_DIR}/manifests/20-control-plane.yaml"

echo "==> Waiting for control-plane Deployment to become Available"
kubectl wait --namespace "${NAMESPACE}" \
    --for=condition=Available deployment/control-plane \
    --timeout=180s

echo "==> Minting a reusable node enrollment token via the control plane"
TOKEN_OUTPUT="$(kubectl exec --namespace "${NAMESPACE}" deploy/control-plane -- \
    agentfab node token create \
        --config /etc/agentfab/agents.yaml \
        --data-dir /var/lib/agentfab \
        --reusable \
        --bind-bundle=false \
        --bind-binary=false \
        --description "kind-${CLUSTER_NAME}")"
TOKEN_VALUE="$(echo "${TOKEN_OUTPUT}" | awk '/^[[:space:]]+Token:[[:space:]]/ {print $2; exit}')"
if [[ -z "${TOKEN_VALUE}" ]]; then
    echo "error: could not parse enrollment token from control-plane output:" >&2
    echo "${TOKEN_OUTPUT}" >&2
    exit 1
fi

echo "==> Creating agentfab-node-token Secret"
kubectl create secret generic agentfab-node-token \
    --namespace "${NAMESPACE}" \
    --from-literal=token="${TOKEN_VALUE}" \
    --dry-run=client -o yaml | kubectl apply -f -

echo "==> Applying node Deployment"
kubectl apply -f "${SCRIPT_DIR}/manifests/30-node.yaml"

if [[ "${EXISTING_CLUSTER}" == "true" ]]; then
    echo "==> Restarting in-cluster workloads to pick up ${IMAGE_NAME}"
    kubectl rollout restart deployment/control-plane --namespace "${NAMESPACE}"
    kubectl rollout restart deployment/agentfab-node --namespace "${NAMESPACE}"
    kubectl wait --namespace "${NAMESPACE}" \
        --for=condition=Available deployment/control-plane \
        --timeout=180s
    kubectl wait --namespace "${NAMESPACE}" \
        --for=condition=Available deployment/agentfab-node \
        --timeout=180s

    echo "==> Recreating conductor Pod to pick up ${IMAGE_NAME}"
    kubectl delete pod conductor --namespace "${NAMESPACE}" --ignore-not-found --wait=true
fi

echo "==> Applying conductor Pod"
kubectl apply -f "${SCRIPT_DIR}/manifests/40-conductor.yaml"

echo "==> Waiting for nodes to register with the control plane"
kubectl wait --namespace "${NAMESPACE}" \
    --for=condition=Available deployment/agentfab-node \
    --timeout=180s
kubectl wait --namespace "${NAMESPACE}" \
    --for=condition=Ready pod/conductor \
    --timeout=180s

cat <<EOF

agentfab is up.

  kubectl get pods -n ${NAMESPACE}
  kubectl logs -n ${NAMESPACE} deploy/etcd
  kubectl logs -n ${NAMESPACE} deploy/control-plane
  kubectl logs -n ${NAMESPACE} deploy/agentfab-node

To open an interactive conductor session inside the cluster:

  kubectl exec -it -n ${NAMESPACE} pod/conductor -- \\
      agentfab run \\
          --config /etc/agentfab/agents.yaml \\
          --data-dir /var/lib/agentfab \\
          --external-nodes \\
          --skip-verify

To tear everything down:

  ${SCRIPT_DIR}/down.sh
EOF
