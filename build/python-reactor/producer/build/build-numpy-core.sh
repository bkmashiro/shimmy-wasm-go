#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)
exec python3 "$ROOT/build/python-reactor/producer/tools/build_numpy_profile.py" "$@"
