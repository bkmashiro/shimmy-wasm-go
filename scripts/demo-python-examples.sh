#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/python-reactor-artifact.env
source "${ROOT}/scripts/python-reactor-artifact.env"
HOST="127.0.0.1"
BIN="${SHIMMY_DEMO_BIN:-${ROOT}/bin/shimmy-demo}"
LOG_DIR="${ROOT}/.demo-logs"
mkdir -p "${LOG_DIR}"

need() { command -v "$1" >/dev/null 2>&1; }

port() {
  python3 - <<'PY'
import socket
s = socket.socket(); s.bind(('127.0.0.1', 0)); print(s.getsockname()[1]); s.close()
PY
}

json_post() {
  local base="$1" response="$2" answer="$3" params_json="$4"
  RESPONSE="${response}" ANSWER="${answer}" PARAMS_JSON="${params_json}" BASE="${base}" python3 - <<'PY'
import json, os, subprocess
payload = {
    "response": os.environ["RESPONSE"],
    "answer": os.environ["ANSWER"],
    "params": json.loads(os.environ["PARAMS_JSON"]),
}
cmd = [
    "curl", "-fsS", "-X", "POST", os.environ["BASE"] + "/",
    "-H", "Content-Type: application/json",
    "-H", "Command: eval",
    "--data", json.dumps(payload),
]
print(subprocess.check_output(cmd, text=True))
PY
}

wait_for_health() {
  local pid="$1" base="$2" log="$3" limit="${4:-120}"
  local ok=0
  for _ in $(seq 1 "${limit}"); do
    if ! kill -0 "${pid}" 2>/dev/null; then
      echo "    FAILED: server exited early; log: ${log}" >&2
      sed -n '1,120p' "${log}" >&2 || true
      return 1
    fi
    if curl -fsS "${base}/health" >/dev/null 2>&1; then ok=1; break; fi
    sleep 0.5
  done
  if [[ "${ok}" != 1 ]]; then
    echo "    FAILED: server did not become ready; log: ${log}" >&2
    sed -n '1,120p' "${log}" >&2 || true
    return 1
  fi
}

stop_pid() {
  local pid="$1"
  if kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}" 2>/dev/null || true
    for _ in $(seq 1 20); do kill -0 "${pid}" 2>/dev/null || return 0; sleep 0.1; done
    kill -KILL "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  fi
}

assert_correct() {
  local resp="$1"
  RESP="${resp}" python3 - <<'PY'
import json, os, sys
body = json.loads(os.environ["RESP"])
result = body.get("result", {})
if result.get("is_correct") is not True:
    print("expected result.is_correct=true, got:", json.dumps(body, indent=2), file=sys.stderr)
    sys.exit(1)
PY
}

ensure_reactor_wasm() {
  if [[ ! -f "${SHIMMY_REACTOR_WASM}" ]]; then
    echo "error: pinned Python Reactor artifact is missing: ${SHIMMY_REACTOR_WASM}" >&2
    exit 1
  fi
  printf '%s\n' "${SHIMMY_REACTOR_WASM}"
}

run_plain_reactor() {
  echo
  echo "==> Plain Python route: examples/eval-python via python-reactor"
  echo '    sample: response="3.14159", answer="3.1416", params={"tolerance":0.001}'

  local wasm p base log pid resp
  wasm="$(ensure_reactor_wasm)"
  p="$(port)"; base="http://${HOST}:${p}"; log="${LOG_DIR}/python-plain.log"; rm -f "${log}"
  (
    cd "${ROOT}"
    exec env \
      LOG_LEVEL=error \
      FUNCTION_INTERFACE=wasm \
      FUNCTION_WASM_PROFILE=python-reactor \
      FUNCTION_WASM_MODULE="${wasm}" \
      FUNCTION_WASM_MANIFEST="${SHIMMY_REACTOR_MANIFEST_PATH}" \
      FUNCTION_WASM_PYTHON_SCRIPT="${ROOT}/examples/eval-python/eval.py" \
      FUNCTION_WASM_MAX_MEMORY_PAGES=8192 \
      FUNCTION_MAX_PROCS=1 \
      FUNCTION_WORKER_SEND_TIMEOUT=120s \
      "${BIN}" serve --host "${HOST}" --port "${p}"
  ) >"${log}" 2>&1 &
  pid="$!"
  wait_for_health "${pid}" "${base}" "${log}" 300
  resp="$(json_post "${base}" "3.14159" "3.1416" '{"tolerance":0.001}')"
  echo "${resp}" | python3 -m json.tool
  assert_correct "${resp}"
  stop_pid "${pid}"
  echo "    ✓ plain Python sample accepted"
}

run_numpy_reactor() {
  local wasm p base log pid resp
  wasm="$(ensure_reactor_wasm)"
  p="$(port)"; base="http://${HOST}:${p}"; log="${LOG_DIR}/python-numpy.log"; rm -f "${log}"
  echo
  echo "==> NumPy route: examples/eval-numpy via python-reactor"
  echo '    sample: response="1,2,3.000001", answer="1,2,3", params={"rtol":0.00001}'
  (
    cd "${ROOT}"
    exec env \
      LOG_LEVEL=error \
      FUNCTION_INTERFACE=wasm \
      FUNCTION_WASM_PROFILE=python-reactor \
      FUNCTION_WASM_MODULE="${wasm}" \
      FUNCTION_WASM_MANIFEST="${SHIMMY_REACTOR_MANIFEST_PATH}" \
      FUNCTION_WASM_PYTHON_SCRIPT="${ROOT}/examples/eval-numpy/eval.py" \
      FUNCTION_WASM_MAX_MEMORY_PAGES=8192 \
      FUNCTION_MAX_PROCS=1 \
      FUNCTION_WORKER_SEND_TIMEOUT=120s \
      "${BIN}" serve --host "${HOST}" --port "${p}"
  ) >"${log}" 2>&1 &
  pid="$!"
  wait_for_health "${pid}" "${base}" "${log}" 300
  resp="$(json_post "${base}" "1,2,3.000001" "1,2,3" '{"rtol":0.00001}')"
  echo "${resp}" | python3 -m json.tool
  assert_correct "${resp}"
  stop_pid "${pid}"
  echo "    ✓ NumPy sample accepted"
}

run_scipy_pyodide() {
  if ! need node || ! need npm; then
    echo
    echo "==> Skipping SciPy route: node/npm not installed"
    return 0
  fi

  echo
  echo "==> SciPy route: examples/eval-scipy via Pyodide"
  echo '    sample: one-sample t-test, answer="5.0", params.samples=[4.9,5.1,5.0,5.2,4.8]'
  (cd "${ROOT}/examples/eval-pyodide" && npm install --silent)

  local p base log pid resp params
  p="$(port)"; base="http://${HOST}:${p}"; log="${LOG_DIR}/python-scipy.log"; rm -f "${log}"
  params='{"test":"ttest","samples":[4.9,5.1,5.0,5.2,4.8],"alpha":0.05}'
  (
    cd "${ROOT}"
    exec env \
      LOG_LEVEL=error \
      FUNCTION_INTERFACE=pyodide \
      FUNCTION_PYODIDE_RUNNER="${ROOT}/examples/eval-pyodide/runner.js" \
      FUNCTION_PYODIDE_SCRIPT="${ROOT}/examples/eval-scipy/eval.py" \
      FUNCTION_MAX_PROCS=1 \
      FUNCTION_WORKER_SEND_TIMEOUT=60s \
      "${BIN}" serve --host "${HOST}" --port "${p}"
  ) >"${log}" 2>&1 &
  pid="$!"
  wait_for_health "${pid}" "${base}" "${log}" 240
  resp="$(json_post "${base}" "" "5.0" "${params}")"
  echo "${resp}" | python3 -m json.tool
  assert_correct "${resp}"
  stop_pid "${pid}"
  echo "    ✓ SciPy sample accepted"
}

run_matplotlib_pyodide() {
  if ! need node || ! need npm; then
    echo
    echo "==> Skipping Matplotlib route: node/npm not installed"
    return 0
  fi

  echo
  echo "==> Matplotlib route: real pyplot PNG rendering via Pyodide"
  (cd "${ROOT}/examples/eval-pyodide" && npm install --silent)

  local p base log pid resp
  p="$(port)"; base="http://${HOST}:${p}"; log="${LOG_DIR}/python-matplotlib.log"; rm -f "${log}"
  (
    cd "${ROOT}"
    exec env \
      LOG_LEVEL=error \
      FUNCTION_INTERFACE=pyodide \
      FUNCTION_PYODIDE_RUNNER="${ROOT}/examples/eval-pyodide/runner.js" \
      FUNCTION_PYODIDE_SCRIPT="${ROOT}/examples/eval-matplotlib/eval.py" \
      FUNCTION_PYODIDE_PACKAGES=matplotlib \
      FUNCTION_MAX_PROCS=1 \
      FUNCTION_WORKER_SEND_TIMEOUT=60s \
      "${BIN}" serve --host "${HOST}" --port "${p}"
  ) >"${log}" 2>&1 &
  pid="$!"
  wait_for_health "${pid}" "${base}" "${log}" 240
  resp="$(json_post "${base}" "" "" '{"values":[0,1,4,9]}')"
  echo "${resp}" | python3 -m json.tool
  assert_correct "${resp}"
  stop_pid "${pid}"
  echo "    ✓ Matplotlib PNG rendered"
}

main() {
  local mode="${1:-all}"
  if ! need go || ! need curl || ! need python3; then
    echo "error: go, curl, and python3 are required" >&2
    exit 1
  fi

  case "${mode}" in
    all|reactor-only|pyodide-only) ;;
    -h|--help)
      echo "usage: $0 [all|reactor-only|pyodide-only]" >&2
      exit 0
      ;;
    *)
      echo "usage: $0 [all|reactor-only|pyodide-only]" >&2
      exit 1
      ;;
  esac

  echo "==> Building shimmy demo binary"
  (cd "${ROOT}" && go build -trimpath -buildvcs=false -o "${BIN}" .)

  if [[ "${mode}" != "pyodide-only" ]]; then
    scripts/verify-python-reactor-artifact.sh
  fi

  if [[ "${mode}" == "reactor-only" ]]; then
    run_plain_reactor
    run_numpy_reactor
    echo
    echo "✅ Python Reactor example demos completed. Logs: ${LOG_DIR}"
    return 0
  fi

  if [[ "${mode}" == "pyodide-only" ]]; then
    run_scipy_pyodide
    run_matplotlib_pyodide
    echo
    echo "✅ Pyodide example demo completed. Logs: ${LOG_DIR}"
    return 0
  fi

  run_plain_reactor
  run_numpy_reactor
  run_scipy_pyodide
  run_matplotlib_pyodide

  echo
  echo "✅ Python example demos completed. Logs: ${LOG_DIR}"
}

main "$@"
