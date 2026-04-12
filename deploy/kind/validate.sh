#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-agentfab-test}"
NAMESPACE="${NAMESPACE:-agentfab}"
CONDUCTOR_POD="${CONDUCTOR_POD:-conductor}"
NODE_DEPLOYMENT="${NODE_DEPLOYMENT:-agentfab-node}"
CONTROL_PLANE_DEPLOYMENT="${CONTROL_PLANE_DEPLOYMENT:-control-plane}"
ETCD_DEPLOYMENT="${ETCD_DEPLOYMENT:-etcd}"
HOST_STATE_DIR="${HOST_STATE_DIR:-${SCRIPT_DIR}/state/${CLUSTER_NAME}}"
FABRIC_SHARED_DIR="${FABRIC_SHARED_DIR:-${HOST_STATE_DIR}/fabric/shared}"
LOG_DIR="${LOG_DIR:-${FABRIC_SHARED_DIR}/logs}"
REQUEST_ARTIFACT_DIR="${REQUEST_ARTIFACT_DIR:-${FABRIC_SHARED_DIR}/artifacts/_requests}"
VALIDATION_DIR="${VALIDATION_DIR:-${HOST_STATE_DIR}/validation}"
REQUEST_HOLD_SECONDS="${REQUEST_HOLD_SECONDS:-180}"
WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-300}"
SMOKE_REQUEST="${SMOKE_REQUEST:-Create a to-do web app with daily, weekly, and monthly recurrence support. Choose a sensible local-first persistence model and produce the working implementation artifacts.}"
FAILOVER_REQUEST="${FAILOVER_REQUEST:-Create a browser-based task planner with daily, weekly, and monthly recurrence support, a polished task list UI, and a working implementation. Produce the full implementation artifacts.}"

MODE="${1:-smoke}"

require() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "error: required tool '$1' not found in PATH" >&2
        exit 1
    fi
}

require kubectl
require grep
require sed
require tee

mkdir -p "${VALIDATION_DIR}"

now_utc() {
    date -u +"%Y-%m-%dT%H:%M:%SZ"
}

latest_status_file() {
    ls -1t "${LOG_DIR}"/*_status.json 2>/dev/null | head -1 || true
}

latest_log_file() {
    ls -1t "${LOG_DIR}"/*.jsonl 2>/dev/null | head -1 || true
}

request_id_from_status_file() {
    local path="$1"
    local base
    base="$(basename "${path}")"
    echo "${base%_status.json}"
}

request_results_file() {
    local request_id="$1"
    echo "${REQUEST_ARTIFACT_DIR}/${request_id}/results.md"
}

wait_for_file_change() {
    local previous="$1"
    local deadline=$((SECONDS + WAIT_TIMEOUT_SECONDS))
    local current
    while (( SECONDS < deadline )); do
        current="$(latest_status_file)"
        if [[ -n "${current}" && "${current}" != "${previous}" ]]; then
            echo "${current}"
            return 0
        fi
        sleep 2
    done
    return 1
}

wait_for_log_pattern() {
    local file="$1"
    local pattern="$2"
    local deadline=$((SECONDS + WAIT_TIMEOUT_SECONDS))
    while (( SECONDS < deadline )); do
        if [[ -f "${file}" ]] && grep -q "${pattern}" "${file}"; then
            return 0
        fi
        sleep 2
    done
    return 1
}

wait_for_request_terminal() {
    local status_file="$1"
    local deadline=$((SECONDS + WAIT_TIMEOUT_SECONDS))
    while (( SECONDS < deadline )); do
        if [[ -f "${status_file}" ]] && ! grep -Eq '"status": "(running|pending|paused)"' "${status_file}"; then
            return 0
        fi
        sleep 2
    done
    return 1
}

ready_replica_count() {
    kubectl get deploy -n "${NAMESPACE}" "${NODE_DEPLOYMENT}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0"
}

desired_replica_count() {
    kubectl get deploy -n "${NAMESPACE}" "${NODE_DEPLOYMENT}" -o jsonpath='{.spec.replicas}'
}

wait_for_node_pool_ready() {
    local desired
    desired="$(desired_replica_count)"
    local deadline=$((SECONDS + WAIT_TIMEOUT_SECONDS))
    while (( SECONDS < deadline )); do
        local ready
        ready="$(ready_replica_count)"
        if [[ "${ready:-0}" == "${desired}" ]]; then
            return 0
        fi
        sleep 2
    done
    return 1
}

emit_request_session_input() {
    local request="$1"
    local interval=5
    local remaining="${REQUEST_HOLD_SECONDS}"

    printf '1\n'
    printf '%s\n' "${request}"

    while (( remaining > 0 )); do
        sleep "${interval}"
        printf 'q\n'
        remaining=$((remaining - interval))
    done
}

cluster_check() {
    [[ -d "${HOST_STATE_DIR}" ]] || {
        echo "error: kind state directory not found at ${HOST_STATE_DIR}" >&2
        exit 1
    }

    kubectl get ns "${NAMESPACE}" >/dev/null
    kubectl wait -n "${NAMESPACE}" --for=condition=Available "deployment/${ETCD_DEPLOYMENT}" --timeout=120s >/dev/null
    kubectl wait -n "${NAMESPACE}" --for=condition=Available "deployment/${CONTROL_PLANE_DEPLOYMENT}" --timeout=120s >/dev/null
    kubectl wait -n "${NAMESPACE}" --for=condition=Available "deployment/${NODE_DEPLOYMENT}" --timeout=120s >/dev/null
    kubectl wait -n "${NAMESPACE}" --for=condition=Ready "pod/${CONDUCTOR_POD}" --timeout=120s >/dev/null

    [[ -d "${LOG_DIR}" ]] || mkdir -p "${LOG_DIR}"
    [[ -d "${REQUEST_ARTIFACT_DIR}" ]] || mkdir -p "${REQUEST_ARTIFACT_DIR}"
}

run_request_session_sync() {
    local request="$1"
    local output_file="$2"
    local fifo
    fifo="$(mktemp -u)"
    mkfifo "${fifo}"

    emit_request_session_input "${request}" >"${fifo}" &
    local writer_pid=$!

    set +e
    kubectl exec -i -n "${NAMESPACE}" "pod/${CONDUCTOR_POD}" -- /bin/sh -lc \
        'exec agentfab run --config /etc/agentfab/agents.yaml --data-dir /var/lib/agentfab --external-nodes --skip-verify' \
        <"${fifo}" | tee "${output_file}"
    local session_status=${PIPESTATUS[0]}
    set -e

    wait "${writer_pid}" || true
    rm -f "${fifo}"
    return "${session_status}"
}

start_request_session_async() {
    local request="$1"
    local output_file="$2"

    REQUEST_FIFO="$(mktemp -u)"
    REQUEST_STATUS_FILE="$(mktemp)"
    mkfifo "${REQUEST_FIFO}"

    emit_request_session_input "${request}" >"${REQUEST_FIFO}" &
    REQUEST_WRITER_PID=$!

    (
        set +e
        kubectl exec -i -n "${NAMESPACE}" "pod/${CONDUCTOR_POD}" -- /bin/sh -lc \
            'exec agentfab run --config /etc/agentfab/agents.yaml --data-dir /var/lib/agentfab --external-nodes --skip-verify' \
            <"${REQUEST_FIFO}" >"${output_file}" 2>&1
        echo $? >"${REQUEST_STATUS_FILE}"
    ) &
    REQUEST_SESSION_PID=$!
}

finish_request_session_async() {
    wait "${REQUEST_SESSION_PID}"
    wait "${REQUEST_WRITER_PID}" || true
    rm -f "${REQUEST_FIFO}"
    local status
    status="$(cat "${REQUEST_STATUS_FILE}")"
    rm -f "${REQUEST_STATUS_FILE}"
    return "${status}"
}

print_success_summary() {
    local name="$1"
    local request_id="$2"
    local output_file="$3"

    echo
    echo "[ok] ${name}"
    echo "  request_id: ${request_id}"
    echo "  status:     ${LOG_DIR}/${request_id}_status.json"
    echo "  results:    $(request_results_file "${request_id}")"
    echo "  output:     ${output_file}"
}

run_smoke() {
    cluster_check

    local previous_status
    previous_status="$(latest_status_file)"
    local output_file="${VALIDATION_DIR}/smoke-$(date +%Y%m%d-%H%M%S).log"

    echo "[${MODE}] $(now_utc) cluster healthy, starting smoke request"
    run_request_session_sync "${SMOKE_REQUEST}" "${output_file}"

    local status_file
    status_file="$(wait_for_file_change "${previous_status}")" || {
        echo "error: no new request status file appeared under ${LOG_DIR}" >&2
        exit 1
    }

    wait_for_request_terminal "${status_file}" || {
        echo "error: request never reached a terminal task state" >&2
        exit 1
    }

    local request_id
    request_id="$(request_id_from_status_file "${status_file}")"
    local results_file
    results_file="$(request_results_file "${request_id}")"

    [[ -f "${results_file}" ]] || {
        echo "error: expected request results artifact at ${results_file}" >&2
        exit 1
    }

    if grep -q '"status": "failed"' "${status_file}"; then
        echo "error: smoke request finished with failed tasks" >&2
        exit 1
    fi

    print_success_summary "smoke request completed" "${request_id}" "${output_file}"
}

extract_first_execution_node() {
    local log_file="$1"
    grep -o '"execution_node":"[^"]*"' "${log_file}" | head -1 | cut -d'"' -f4
}

run_failover() {
    cluster_check

    local previous_status
    previous_status="$(latest_status_file)"
    local output_file="${VALIDATION_DIR}/failover-$(date +%Y%m%d-%H%M%S).log"

    echo "[${MODE}] $(now_utc) starting failover request"
    start_request_session_async "${FAILOVER_REQUEST}" "${output_file}"

    local status_file
    status_file="$(wait_for_file_change "${previous_status}")" || {
        echo "error: no new request status file appeared under ${LOG_DIR}" >&2
        exit 1
    }
    local request_id
    request_id="$(request_id_from_status_file "${status_file}")"
    local log_file="${LOG_DIR}/${request_id}.jsonl"

    wait_for_log_pattern "${log_file}" '"type":"task_assignment"' || {
        echo "error: request never produced a task assignment event" >&2
        finish_request_session_async || true
        exit 1
    }

    local victim
    victim="$(extract_first_execution_node "${log_file}")"
    [[ -n "${victim}" ]] || {
        echo "error: could not determine assigned execution node from ${log_file}" >&2
        finish_request_session_async || true
        exit 1
    }

    echo "[${MODE}] deleting node pod ${victim}"
    kubectl delete pod -n "${NAMESPACE}" "${victim}" --wait=false >/dev/null

    wait_for_node_pool_ready || {
        echo "error: node deployment did not return to the desired ready replica count" >&2
        finish_request_session_async || true
        exit 1
    }

    finish_request_session_async

    wait_for_request_terminal "${status_file}" || {
        echo "error: failover request never reached a terminal task state" >&2
        exit 1
    }

    local results_file
    results_file="$(request_results_file "${request_id}")"
    [[ -f "${results_file}" ]] || {
        echo "error: expected request results artifact at ${results_file}" >&2
        exit 1
    }

    if grep -q '"status": "failed"' "${status_file}"; then
        echo "error: failover request finished with failed tasks" >&2
        exit 1
    fi

    print_success_summary "failover request completed after node deletion" "${request_id}" "${output_file}"
}

run_check() {
    cluster_check
    echo "[ok] cluster healthy"
    echo "  namespace:    ${NAMESPACE}"
    echo "  conductor:    ${CONDUCTOR_POD}"
    echo "  shared state: ${FABRIC_SHARED_DIR}"
    echo "  ready nodes:  $(ready_replica_count)/$(desired_replica_count)"
}

usage() {
    cat <<EOF
Usage: $(basename "$0") [check|smoke|failover]

Modes:
  check     Verify the kind fabric is up and its core workloads are ready.
  smoke     Run one end-to-end distributed request and verify results persisted.
  failover  Run one distributed request, delete the first assigned node pod,
            and verify the request still completes.

Environment overrides:
  CLUSTER_NAME, NAMESPACE, CONDUCTOR_POD, HOST_STATE_DIR
  REQUEST_HOLD_SECONDS, WAIT_TIMEOUT_SECONDS
  SMOKE_REQUEST, FAILOVER_REQUEST
EOF
}

case "${MODE}" in
    check)
        run_check
        ;;
    smoke)
        run_smoke
        ;;
    failover)
        run_failover
        ;;
    -h|--help|help)
        usage
        ;;
    *)
        usage >&2
        exit 1
        ;;
esac
