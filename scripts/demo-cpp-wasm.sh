#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${PORT:-}"
HOST="127.0.0.1"
if [[ -z "${PORT}" ]]; then
  PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
PY
)"
fi
BASE_URL="http://${HOST}:${PORT}"
BIN="${ROOT}/bin/shimmy-demo-cpp"
DEMO_DIR="${ROOT}/examples/demo-cpp-compare"
WASM="${DEMO_DIR}/eval.wasm"
LOG="${ROOT}/.demo-cpp-wasm-server.log"

for cmd in go curl python3 zig file; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "error: ${cmd} is required" >&2
    exit 1
  fi
done

if [[ ! -f "${DEMO_DIR}/evaluator.cpp" ]]; then
  echo "error: missing ${DEMO_DIR}/evaluator.cpp" >&2
  exit 1
fi

echo "==> Building shimmy demo binary"
(cd "${ROOT}" && go build -trimpath -buildvcs=false -o "${BIN}" .)

echo "==> Building C++ evaluator: examples/demo-cpp-compare -> eval.wasm"
zig c++ \
  -target wasm32-freestanding \
  -Oz \
  -nostdlib \
  -fno-exceptions \
  -fno-rtti \
  -Wl,--no-entry \
  -Wl,--export=alloc \
  -Wl,--export=evaluate \
  -Wl,--export-memory \
  -Wl,--initial-memory=2097152 \
  -o "${WASM}" \
  "${DEMO_DIR}/evaluator.cpp"

file "${WASM}"

rm -f "${LOG}"
server_pid=""
cleanup() {
  if [[ -n "${server_pid}" ]] && kill -0 "${server_pid}" 2>/dev/null; then
    kill "${server_pid}" 2>/dev/null || true
    for _ in {1..20}; do
      kill -0 "${server_pid}" 2>/dev/null || return 0
      sleep 0.1
    done
    kill -KILL "${server_pid}" 2>/dev/null || true
    wait "${server_pid}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "==> Starting shimmy on ${BASE_URL}"
(
  cd "${ROOT}"
  exec env \
    LOG_LEVEL=error \
    FUNCTION_INTERFACE=wasm \
    FUNCTION_WASM_MODULE="${WASM}" \
    FUNCTION_MAX_PROCS=1 \
    FUNCTION_TIMEOUT=5s \
    "${BIN}" serve --host "${HOST}" --port "${PORT}"
) >"${LOG}" 2>&1 &
server_pid="$!"

for _ in {1..60}; do
  if ! kill -0 "${server_pid}" 2>/dev/null; then
    echo "server exited early; log follows:" >&2
    cat "${LOG}" >&2 || true
    exit 1
  fi
  if curl -fsS "${BASE_URL}/health" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

if ! curl -fsS "${BASE_URL}/health" >/dev/null 2>&1; then
  echo "server did not become ready; log follows:" >&2
  cat "${LOG}" >&2 || true
  exit 1
fi

request_eval() {
  local response="$1"
  local answer="$2"
  curl -fsS \
    -X POST "${BASE_URL}/" \
    -H 'Content-Type: application/json' \
    -H 'Command: eval' \
    --data "{\"response\":\"${response}\",\"answer\":\"${answer}\",\"params\":{\"correct_response_feedback\":\"Correct!\",\"incorrect_response_feedback\":\"Try again.\"}}"
}

echo "==> Request #1: correct answer"
resp1="$(request_eval 42 42)"
echo "${resp1}" | python3 -m json.tool

echo "==> Request #2: wrong answer; C++ guest global state should still reset"
resp2="$(request_eval 41 42)"
echo "${resp2}" | python3 -m json.tool

RESP1="${resp1}" RESP2="${resp2}" python3 - <<'PY'
import json, os, sys
r1 = json.loads(os.environ['RESP1'])['result']
r2 = json.loads(os.environ['RESP2'])['result']
checks = [
    (r1.get('is_correct') is True, 'request #1 should be correct'),
    (r1.get('feedback') == 'Correct!', 'request #1 should use correct feedback'),
    (r2.get('is_correct') is False, 'request #2 should be incorrect'),
    (r2.get('feedback') == 'Try again.', 'request #2 should use incorrect feedback'),
    (r1.get('guest_invocation_count') == 1, 'request #1 should see guest_invocation_count == 1'),
    (r2.get('guest_invocation_count') == 1, 'request #2 should still see guest_invocation_count == 1'),
    (r1.get('snapshot_isolation_ok') is True, 'request #1 snapshot flag should be true'),
    (r2.get('snapshot_isolation_ok') is True, 'request #2 snapshot flag should be true'),
]
failed = [msg for ok, msg in checks if not ok]
if failed:
    print('DEMO FAILED:', *failed, sep='\n- ', file=sys.stderr)
    sys.exit(1)
print('\n✅ C++ WASM demo passed: Shimmy ran a C++ evaluator compiled to WebAssembly and restored guest state after each request.')
PY

printf '\nServer log: %s\n' "${LOG}"
