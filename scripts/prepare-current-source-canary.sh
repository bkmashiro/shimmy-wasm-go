#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  printf 'usage: %s OUTPUT_DIR\n' "$0" >&2
  exit 2
}

[[ $# -eq 1 ]] || usage
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "$root_dir/scripts/prepare-agent-python-doc-bundle.sh" \
  "$1" \
  "$root_dir/experiments/agent-python-ultimate/configs/current-source-canary.json" \
  20260809
