#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ADAPTER_DIR="${ROOT}/examples/lambda-feedback-adapter"
PYODIDE_RUNNER="${ROOT}/examples/eval-pyodide/runner.js"
LOG_DIR="${ROOT}/.demo-logs"
mkdir -p "${LOG_DIR}"

MODE_SKIPPED=0

need() {
  command -v "$1" >/dev/null 2>&1
}

assert_json_object() {
  local mode="$1"
  local payload="$2"
  MODE="$mode" PAYLOAD="$payload" python3 - <<'PY'
import json
import os
import sys

mode = os.environ["MODE"]
payload = os.environ["PAYLOAD"]

try:
    body = json.loads(payload)
except Exception as exc:
    print(f"[{mode}] response is not valid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

if not isinstance(body, dict):
    print(f"[{mode}] expected JSON object, got: {body}", file=sys.stderr)
    sys.exit(1)
PY
}

assert_has_result() {
  local mode="$1"
  local payload="$2"
  MODE="$mode" PAYLOAD="$payload" python3 - <<'PY'
import json
import os
import sys

mode = os.environ["MODE"]
payload = os.environ["PAYLOAD"]

try:
    body = json.loads(payload)
except Exception as exc:
    print(f"[{mode}] response is not valid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

if "result" not in body:
    print(f"[{mode}] response missing `result`: {body}", file=sys.stderr)
    sys.exit(1)
PY
}

assert_eval_true() {
  local mode="$1"
  local payload="$2"
  MODE="$mode" PAYLOAD="$payload" EXPECT_WRAPPED=1 python3 - <<'PY'
import json
import os
import sys

mode = os.environ["MODE"]
payload = os.environ["PAYLOAD"]
expect_wrapped = os.environ.get("EXPECT_WRAPPED") == "1"

try:
    body = json.loads(payload)
except Exception as exc:
    print(f"[{mode}] response is not valid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

result = body.get("result") if expect_wrapped else body
if not isinstance(result, dict) or result.get("is_correct") is not True:
    print(f"[{mode}] expected result.is_correct=true, got: {body}", file=sys.stderr)
    sys.exit(1)
PY
}

assert_plain_eval_true() {
  local mode="$1"
  local payload="$2"
  MODE="$mode" PAYLOAD="$payload" EXPECT_WRAPPED=0 python3 - <<'PY'
import json
import os
import sys

mode = os.environ["MODE"]
payload = os.environ["PAYLOAD"]
expect_wrapped = os.environ.get("EXPECT_WRAPPED") == "1"

try:
    body = json.loads(payload)
except Exception as exc:
    print(f"[{mode}] response is not valid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

result = body.get("result") if expect_wrapped else body
if not isinstance(result, dict) or result.get("is_correct") is not True:
    print(f"[{mode}] expected result.is_correct=true, got: {body}", file=sys.stderr)
    sys.exit(1)
PY
}

run_local_boilerplate() {
  echo "==> local-boilerplate"
  local root="${ROOT}/examples/lambda-feedback-fixtures/boilerplate-python"
  local eval_entry="evaluation_function.evaluation:evaluation_function"
  local preview_entry="evaluation_function.preview:preview_function"

  local input_eval='{ "response": "2", "answer": "2", "params": {} }'
  local input_preview='{ "response": "2", "answer": "2", "params": {} }'

  local eval_json
  if ! eval_json="$(python3 "${ADAPTER_DIR}/run_lf_eval.py" \
      --root "${root}" \
      --eval-entrypoint "${eval_entry}" \
      --preview-entrypoint "${preview_entry}" \
      --method eval \
      --input "${input_eval}")"; then
    return 1
  fi
  assert_plain_eval_true "local-boilerplate(eval)" "${eval_json}"
  echo "    PASS eval"

  local preview_json
  if ! preview_json="$(python3 "${ADAPTER_DIR}/run_lf_eval.py" \
      --root "${root}" \
      --eval-entrypoint "${eval_entry}" \
      --preview-entrypoint "${preview_entry}" \
      --method preview \
      --input "${input_preview}")"; then
    return 1
  fi
  assert_json_object "local-boilerplate(preview)" "${preview_json}"
  echo "    PASS preview"
}

run_local_compare_boolean() {
  if ! need python3; then
    echo "==> local-compare-boolean (skipped): python3 missing"
    MODE_SKIPPED=1
    return 0
  fi

  if ! python3 - <<'PY'
import importlib.util
raise SystemExit(0 if importlib.util.find_spec("sympy") else 1)
PY
  then
    echo "==> local-compare-boolean (skipped): sympy not installed"
    MODE_SKIPPED=1
    return 0
  fi

  echo "==> local-compare-boolean"
  local root="${ROOT}/examples/lambda-feedback-fixtures/compare-boolean"
  local eval_entry="evaluation_function.evaluation:evaluation_function"
  local preview_entry="evaluation_function.preview:preview_function"

  local input_eval='{ "response": "A", "answer": "A", "params": {"disallowed": []} }'
  local input_preview='{ "response": "A", "answer": "A", "params": {"disallowed": []} }'

  local eval_json
  if ! eval_json="$(python3 "${ADAPTER_DIR}/run_lf_eval.py" \
      --root "${root}" \
      --eval-entrypoint "${eval_entry}" \
      --preview-entrypoint "${preview_entry}" \
      --method eval \
      --input "${input_eval}")"; then
    return 1
  fi
  assert_plain_eval_true "local-compare-boolean(eval)" "${eval_json}"
  echo "    PASS eval"

  local preview_json
  if ! preview_json="$(python3 "${ADAPTER_DIR}/run_lf_eval.py" \
      --root "${root}" \
      --eval-entrypoint "${eval_entry}" \
      --preview-entrypoint "${preview_entry}" \
      --method preview \
      --input "${input_preview}")"; then
    return 1
  fi
  assert_json_object "local-compare-boolean(preview)" "${preview_json}"
  echo "    PASS preview"
}

run_pyodide_rpc_request() {
  local root="$1"
  local eval_entry="$2"
  local preview_entry="$3"
  local method="$4"
  local response="$5"
  local answer="$6"
  local params_json="$7"
  local packages="$8"

  if ! need node; then
    echo "==> ${method} via pyodide (skipped): node missing"
    MODE_SKIPPED=1
    return 0
  fi

  local result_json
  if ! result_json="$(PYODIDE_ROOT="${root}" \
      PYODIDE_ENTRY="${eval_entry}" \
      PYODIDE_PREVIEW="${preview_entry}" \
      PYODIDE_PACKAGES="${packages}" \
      PYODIDE_ADAPTER="${ADAPTER_DIR}/lf_compat_adapter.py" \
      PYODIDE_METHOD="${method}" \
      PYODIDE_RESPONSE="${response}" \
      PYODIDE_ANSWER="${answer}" \
      PYODIDE_PARAMS_JSON="${params_json}" \
      PYODIDE_RUNNER="${PYODIDE_RUNNER}" \
      python3 - <<'PY'
import json
import os
import subprocess
import sys

runner = os.environ["PYODIDE_RUNNER"]
root = os.environ["PYODIDE_ROOT"]
eval_entry = os.environ["PYODIDE_ENTRY"]
preview_entry = os.environ["PYODIDE_PREVIEW"]
method = os.environ["PYODIDE_METHOD"]
response = os.environ["PYODIDE_RESPONSE"]
answer = os.environ["PYODIDE_ANSWER"]
params = json.loads(os.environ["PYODIDE_PARAMS_JSON"])
packages = os.environ["PYODIDE_PACKAGES"]

request = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": method,
    "params": [
        {
            "response": response,
            "answer": answer,
            "params": params,
        }
    ],
}

payload = json.dumps(request, separators=(",", ":")).encode("utf-8")
frame = f"Content-Length: {len(payload)}\r\n\r\n".encode("ascii") + payload

env = os.environ.copy()
env.update(
    {
        "FUNCTION_PYODIDE_ROOT": root,
        "FUNCTION_PYODIDE_EVAL_ENTRYPOINT": eval_entry,
        "FUNCTION_PYODIDE_PREVIEW_ENTRYPOINT": preview_entry,
        "FUNCTION_PYODIDE_ADAPTER": os.environ["PYODIDE_ADAPTER"],
        "FUNCTION_PYODIDE_PACKAGES": packages,
    }
)

proc = subprocess.Popen(
    ["node", runner],
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    env=env,
)

try:
    proc.stdin.write(frame)
    proc.stdin.flush()

    header = b""
    while True:
        line = proc.stdout.readline()
        if not line:
            raise RuntimeError("runner exited before sending response frame")
        if line.startswith(b"Content-Length:"):
            header = line
            break

    content_length = int(header.split(b":", 1)[1].strip())
    while True:
        sep = proc.stdout.readline()
        if sep in (b"\r\n", b"\n", b""):
            break

    body = proc.stdout.read(content_length)
    text = body.decode("utf-8")

    try:
        proc.stdin.close()
    except Exception:
        pass
    try:
        proc.terminate()
    except Exception:
        pass
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=5)

    response_obj = json.loads(text)
except Exception as exc:  # pragma: no cover
    try:
        proc.kill()
    except Exception:
        pass
    try:
        proc.wait(timeout=5)
    except Exception:
        pass
    print(f"Error communicating with pyodide runner: {exc}", file=sys.stderr)
    sys.exit(1)

if response_obj.get("error") is not None:
    print(f"Runner returned error frame: {response_obj}", file=sys.stderr)
    sys.exit(1)

if "result" not in response_obj:
    print(f"Runner response missing result: {response_obj}", file=sys.stderr)
    sys.exit(1)

print(json.dumps(response_obj))
PY
  )"; then
    return 1
  fi

  echo "${result_json}"
}

run_pyodide_boilerplate() {
  if ! need node; then
    echo "==> pyodide-boilerplate (skipped): node missing"
    MODE_SKIPPED=1
    return 0
  fi

  if [[ ! -d "${ROOT}/examples/eval-pyodide/node_modules/pyodide" ]]; then
    echo "==> pyodide-boilerplate (skipped): node_modules/pyodide missing"
    MODE_SKIPPED=1
    return 0
  fi

  local root="${ROOT}/examples/lambda-feedback-fixtures/boilerplate-python"
  local eval_entry="evaluation_function.evaluation:evaluation_function"
  local preview_entry=""
  local params='{}'

  local response_json
  if ! response_json="$(run_pyodide_rpc_request "${root}" "${eval_entry}" "${preview_entry}" "evaluate" "2" "2" "${params}" "")"; then
    return 1
  fi
  assert_has_result "pyodide-boilerplate" "${response_json}"
  echo "    PASS"
}

run_pyodide_compare_boolean() {
  if ! need node; then
    echo "==> pyodide-compare-boolean (skipped): node missing"
    MODE_SKIPPED=1
    return 0
  fi

  if [[ ! -d "${ROOT}/examples/eval-pyodide/node_modules/pyodide" ]]; then
    echo "==> pyodide-compare-boolean (skipped): node_modules/pyodide missing"
    MODE_SKIPPED=1
    return 0
  fi

  local root="${ROOT}/examples/lambda-feedback-fixtures/compare-boolean"
  local eval_entry="evaluation_function.evaluation:evaluation_function"
  local preview_entry=""
  local params='{"disallowed": []}'

  local response_json
  if ! response_json="$(run_pyodide_rpc_request "${root}" "${eval_entry}" "${preview_entry}" "evaluate" "A" "A" "${params}" "sympy")"; then
    return 1
  fi
  assert_has_result "pyodide-compare-boolean" "${response_json}"
  echo "    PASS"
}

run_pyodide_short_text() {
  if ! need node; then
    echo "==> pyodide-short-text (skipped): node missing"
    MODE_SKIPPED=1
    return 0
  fi

  if [[ ! -d "${ROOT}/examples/eval-pyodide/node_modules/pyodide" ]]; then
    echo "==> pyodide-short-text (skipped): node_modules/pyodide missing"
    MODE_SKIPPED=1
    return 0
  fi

  local root="${LF_SHORT_TEXT_BUNDLE:-${ROOT}/.demo-pyodide-bundles/short-text-answer}"
  if [[ ! -f "${root}/evaluation.py" || ! -d "${root}/nltk_data" ]]; then
    echo "error: prepared shortTextAnswer bundle missing: ${root}" >&2
    echo "prepare it with scripts/prepare-short-text-pyodide-bundle.py" >&2
    return 1
  fi

  local response_json
  if ! response_json="$(run_pyodide_rpc_request \
      "${root}" \
      "evaluation:evaluation_function" \
      "" \
      "evaluate" \
      "A xor gate takes 2 inputs" \
      "There are 2 inputs in a xor gate" \
      "{}" \
      "gensim,nltk,matplotlib")"; then
    return 1
  fi
  assert_eval_true "pyodide-short-text" "${response_json}"
  echo "    PASS"
}

run_list() {
  echo "Available modes:"
  echo "  list"
  echo "  local-boilerplate"
  echo "  local-compare-boolean"
  echo "  pyodide-boilerplate"
  echo "  pyodide-compare-boolean"
  echo "  pyodide-short-text"
  echo "  all"
}

run_all() {
  local failed=0
  local summaries=()

  local mode
  local status

  mode="local-boilerplate"
  MODE_SKIPPED=0
  if run_local_boilerplate; then
    status="PASS"
    if [[ "${MODE_SKIPPED}" -eq 1 ]]; then
      status="SKIP"
    fi
  else
    status="FAIL"
    failed=1
  fi
  summaries+=("${mode}:${status}")

  mode="local-compare-boolean"
  MODE_SKIPPED=0
  if run_local_compare_boolean; then
    status="PASS"
    if [[ "${MODE_SKIPPED}" -eq 1 ]]; then
      status="SKIP"
    fi
  else
    status="FAIL"
    failed=1
  fi
  summaries+=("${mode}:${status}")

  mode="pyodide-boilerplate"
  MODE_SKIPPED=0
  if run_pyodide_boilerplate; then
    status="PASS"
    if [[ "${MODE_SKIPPED}" -eq 1 ]]; then
      status="SKIP"
    fi
  else
    status="FAIL"
    failed=1
  fi
  summaries+=("${mode}:${status}")

  mode="pyodide-compare-boolean"
  MODE_SKIPPED=0
  if run_pyodide_compare_boolean; then
    status="PASS"
    if [[ "${MODE_SKIPPED}" -eq 1 ]]; then
      status="SKIP"
    fi
  else
    status="FAIL"
    failed=1
  fi
  summaries+=("${mode}:${status}")

  printf '\nPASS summary: '
  local idx
  for idx in "${!summaries[@]}"; do
    if (( idx > 0 )); then
      printf ' | '
    fi
    printf '%s' "${summaries[idx]}"
  done
  echo

  if [[ "${failed}" -ne 0 ]]; then
    return 1
  fi
}

main() {
  if ! need python3; then
    echo "error: python3 is required" >&2
    exit 1
  fi

  local mode="${1:-list}"

  case "${mode}" in
    list)
      run_list
      ;;
    local-boilerplate)
      run_local_boilerplate
      ;;
    local-compare-boolean)
      run_local_compare_boolean
      ;;
    pyodide-boilerplate)
      run_pyodide_boilerplate
      ;;
    pyodide-compare-boolean)
      run_pyodide_compare_boolean
      ;;
    pyodide-short-text)
      run_pyodide_short_text
      ;;
    all)
      run_all
      ;;
    *)
      echo "Unknown mode: ${mode}" >&2
      run_list >&2
      exit 1
      ;;
  esac
}

main "$@"
