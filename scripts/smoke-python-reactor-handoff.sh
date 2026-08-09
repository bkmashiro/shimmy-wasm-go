#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="${1:-artifact-only}"
# shellcheck source=scripts/python-reactor-artifact.env
source "${ROOT}/scripts/python-reactor-artifact.env"

usage() {
  cat >&2 <<'EOF'
usage: scripts/smoke-python-reactor-handoff.sh [artifact-only|direct]

artifact-only  Verify the pinned bundle, protocol tests, routing, and Linux compile.
direct         Also execute compatibility, NumPy binary128, denial, and timeout recovery.
EOF
}

case "${MODE}" in
  artifact-only|direct) ;;
  -h|--help) usage; exit 0 ;;
  *) usage; exit 1 ;;
esac

cd "${ROOT}"

echo "==> verifying pinned Python Reactor runtime bundle"
scripts/verify-python-reactor-artifact.sh

echo "==> shell syntax checks"
bash -n scripts/demo-python-examples.sh \
  scripts/smoke-python-reactor-handoff.sh \
  scripts/verify-python-reactor-artifact.sh

echo "==> protocol and dispatcher routing tests"
go test ./internal/execution/wasm \
  -run='Test(VerifyAgentPython|BuildAgentPython|DecodeAgentPython)' \
  -count=1
go test ./internal/execution \
  -run='TestNewDispatcher_.*Wasm|TestNewDispatcher_.*Reactor|TestScriptRouting' \
  -count=1

echo "==> Linux compile gate"
GOOS=linux GOARCH=amd64 go test -c ./internal/execution/wasm -o /tmp/shimmy-wasm-python-reactor.test
rm -f /tmp/shimmy-wasm-python-reactor.test

if [[ "${MODE}" == "direct" ]]; then
  echo "==> real Python Reactor runtime E2E"
  AGENT_PYTHON_RUNTIME_WASM="${SHIMMY_REACTOR_WASM}" \
  AGENT_PYTHON_RUNTIME_MANIFEST="${SHIMMY_REACTOR_MANIFEST_PATH}" \
    go test ./internal/execution/wasm \
      -run='^TestAgentPythonDispatcher(RealNumPyArtifactCompatibility|RealNumPyCOWRestoresState|SingleUsePreparedRefillsNeverServedCandidates|TimeoutDoesNotPoisonRuntime|RealLambdaFeedbackBundle)$' \
      -count=1 -v -timeout=15m
fi

echo "PASS: Python Reactor runtime handoff smoke (${MODE})"
