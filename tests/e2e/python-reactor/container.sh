#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
IMAGE="${SHIMMY_E2E_CONTAINER_IMAGE:?set SHIMMY_E2E_CONTAINER_IMAGE}"
WASM="${SHIMMY_PYTHON_REACTOR_WASM:?set SHIMMY_PYTHON_REACTOR_WASM}"
MANIFEST="${SHIMMY_PYTHON_REACTOR_MANIFEST:?set SHIMMY_PYTHON_REACTOR_MANIFEST}"
EVALUATOR="${SHIMMY_E2E_EVALUATOR:-${ROOT}/tests/e2e/python-reactor/evaluator.py}"
WASM_NAME="$(basename "${WASM}")"
NAME="shimmy-python-reactor-e2e-${GITHUB_RUN_ID:-$$}-${GITHUB_RUN_ATTEMPT:-1}"
PORT="${SHIMMY_E2E_PORT:-18080}"

for cmd in curl docker python3; do
  command -v "${cmd}" >/dev/null 2>&1 || { echo "missing required command: ${cmd}" >&2; exit 1; }
done
[[ -r "${WASM}" && -r "${MANIFEST}" && -r "${EVALUATOR}" ]] || {
  echo "artifact, manifest, and evaluator must be readable" >&2
  exit 1
}

cleanup() {
  docker rm -f "${NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

cleanup
# Do not use --rm: when startup fails, the exit trap removes the container after
# we have captured its logs and terminal state.
docker run -d \
  --name "${NAME}" \
  --publish "127.0.0.1:${PORT}:8080" \
  --volume "${WASM}:/runtime/${WASM_NAME}:ro" \
  --volume "${MANIFEST}:/runtime/manifest.json:ro" \
  --volume "${EVALUATOR}:/runtime/evaluator.py:ro" \
  --env LOG_LEVEL=error \
  --env FUNCTION_INTERFACE=wasm \
  --env FUNCTION_WASM_PROFILE=python-reactor \
  --env FUNCTION_WASM_MODULE="/runtime/${WASM_NAME}" \
  --env FUNCTION_WASM_MANIFEST=/runtime/manifest.json \
  --env FUNCTION_WASM_PYTHON_SCRIPT=/runtime/evaluator.py \
  --env FUNCTION_WASM_PYTHON_LIFECYCLE=snapshot \
  --env FUNCTION_WASM_PYTHON_PREPARE_TIMEOUT=3m \
  --env FUNCTION_WASM_MAX_MEMORY_PAGES=8192 \
  --env FUNCTION_MAX_PROCS=1 \
  --env FUNCTION_WORKER_SEND_TIMEOUT=30s \
  "${IMAGE}" serve --host 0.0.0.0 --port 8080 >/dev/null

BASE_URL="http://127.0.0.1:${PORT}"
ready=false
for _ in $(seq 1 300); do
  if ! docker inspect --format '{{.State.Running}}' "${NAME}" 2>/dev/null | grep -qx true; then
    echo "Shimmy container exited during startup" >&2
    docker logs "${NAME}" >&2 || true
    exit 1
  fi
  if curl -fsS "${BASE_URL}/health" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.2
done
if [[ "${ready}" != true ]]; then
  echo "Shimmy container did not become ready" >&2
  docker logs "${NAME}" >&2 || true
  exit 1
fi

request() {
  curl -fsS -X POST "${BASE_URL}/" \
    -H 'Content-Type: application/json' \
    -H "Command: $1" \
    --data "$2"
}

EVAL_OK="$(request eval '{"response":"42","answer":"42","params":{"tolerance":0}}')"
PREVIEW="$(request preview '{"response":"41","params":{}}')"
EVAL_OK="${EVAL_OK}" PREVIEW="${PREVIEW}" python3 - <<'PY'
import json
import os

def result(name):
    body = json.loads(os.environ[name])
    if "error" in body:
        raise SystemExit(f"{name} returned an error: {body['error']}")
    return body["result"]

ok = result("EVAL_OK")
preview = result("PREVIEW")
if ok.get("is_correct") is not True:
    raise SystemExit(f"unexpected eval result: {ok!r}")
if preview.get("preview") != "submitted: 41":
    raise SystemExit(f"unexpected preview result: {preview!r}")
print(json.dumps({"eval": ok, "preview": preview}, sort_keys=True))
PY

printf 'PASS: containerized Python Reactor HTTP E2E\n'
