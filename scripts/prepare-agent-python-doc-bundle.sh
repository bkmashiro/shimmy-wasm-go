#!/usr/bin/env bash
set -euo pipefail
umask 077

for name in CI GITHUB_ACTIONS GITLAB_CI BUILDKITE CIRCLECI JENKINS_URL; do
  value="${!name-}"
  case "$value" in
    ""|0|false|FALSE|no|NO|off|OFF) ;;
    *)
      printf 'Agent Python DoC bundle preparation is manual-only; refusing CI environment (%s)\n' "$name" >&2
      exit 2
      ;;
  esac
done

usage() {
  printf 'usage: %s OUTPUT_DIR CONFIG_JSON PLAN_SEED\n' "$0" >&2
  exit 2
}

[[ $# -eq 3 ]] || usage
output_dir="$1"
config_path="$2"
plan_seed="$3"
[[ "$plan_seed" =~ ^[1-9][0-9]*$ ]] || { printf 'invalid plan seed: %q\n' "$plan_seed" >&2; exit 2; }

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"
config_path="$(python3 -c 'import os,sys; print(os.path.abspath(sys.argv[1]))' "$config_path")"
output_dir="$(python3 -c 'import os,sys; print(os.path.abspath(sys.argv[1]))' "$output_dir")"
[[ -s "$config_path" ]] || { printf 'missing config: %s\n' "$config_path" >&2; exit 2; }

git diff --quiet -- . ':!.hermes' || { printf 'tracked worktree changes must be committed first\n' >&2; exit 2; }
git diff --cached --quiet -- . ':!.hermes' || { printf 'staged changes must be committed first\n' >&2; exit 2; }
untracked="$(git ls-files --others --exclude-standard -- . ':(exclude).hermes/**')"
[[ -z "$untracked" ]] || { printf 'untracked source files must be committed or removed first\n' >&2; exit 2; }
source_commit="$(git rev-parse HEAD)"
[[ "$source_commit" =~ ^[0-9a-f]{40}$ ]] || exit 2
git verify-commit "$source_commit" >/dev/null
config_rel="$(python3 - "$root_dir" "$config_path" <<'PY'
import pathlib
import sys
try:
    print(pathlib.Path(sys.argv[2]).resolve().relative_to(pathlib.Path(sys.argv[1]).resolve()))
except ValueError:
    raise SystemExit("config must be inside the repository")
PY
)"
git ls-files --error-unmatch -- "$config_rel" >/dev/null

artifact="$root_dir/build/python-reactor/artifacts/agent-python-runtime-numpy-core.wasm"
manifest="$root_dir/build/python-reactor/artifacts/manifest.json"
for path in "$artifact" "$manifest"; do
  [[ -s "$path" ]] || { printf 'missing required input: %s\n' "$path" >&2; exit 2; }
done

parent="$(dirname "$output_dir")"
mkdir -p -m 0700 -- "$parent"
temp_dir="$(mktemp -d "$parent/.agent-python-bundle.XXXXXX")"
cleanup() { rm -rf -- "$temp_dir"; }
trap cleanup EXIT
stage="$temp_dir/input"
source_tree="$temp_dir/source"
mkdir -m 0700 -- "$stage" "$source_tree"
git archive --format=tar "$source_commit" | tar -xf - -C "$source_tree"

(
  cd "$source_tree"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-X main.sourceCommit=$source_commit" \
    -o "$stage/agent-python-ultimate" ./experiments/agent-python-ultimate
)
cp -- "$source_tree/$config_rel" "$stage/ultimate.json"
cp -- "$artifact" "$stage/agent-python-runtime-numpy-core.wasm"
cp -- "$manifest" "$stage/manifest.json"
printf '%s\n' "$plan_seed" >"$stage/plan-seed.txt"

(
  cd "$source_tree"
  go run ./experiments/agent-python-ultimate plan \
    --config "$stage/ultimate.json" --seed "$plan_seed" --output "$stage/plan.preview.json"
)

python3 - "$stage" "$source_commit" "$config_rel" <<'PY'
import hashlib
import json
import pathlib
import sys

stage = pathlib.Path(sys.argv[1])
commit = sys.argv[2]
config_path = sys.argv[3]
files = {}
for path in sorted(stage.iterdir()):
    if path.is_file():
        files[path.name] = {
            "bytes": path.stat().st_size,
            "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
        }
manifest = {
    "schema": "shimmy-agent-python-doc-input/v1",
    "source_commit": commit,
    "config_path": config_path,
    "plan_seed": int((stage / "plan-seed.txt").read_text().strip()),
    "files": files,
}
(stage / "input-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
PY

(
  cd "$stage"
  shasum -a 256 agent-python-ultimate ultimate.json agent-python-runtime-numpy-core.wasm \
    manifest.json plan-seed.txt plan.preview.json input-manifest.json >input-files.sha256
)

(
  cd "$stage"
  tar -cf - agent-python-ultimate ultimate.json agent-python-runtime-numpy-core.wasm \
    manifest.json plan-seed.txt plan.preview.json input-manifest.json input-files.sha256 \
    | zstd -q -T0 -19 -o "$temp_dir/input.tar.zst"
)
(
  cd "$temp_dir"
  shasum -a 256 input.tar.zst | sed 's/  /  /' >input.sha256
)
cp -- "$source_tree/experiments/agent-python-ultimate/slurm/job.sh" "$temp_dir/job.sh"
cp -- "$source_tree/experiments/agent-python-ultimate/scripts/safe_extract_tar_zst.py" "$temp_dir/safe-extract-tar-zst.py"
chmod 0700 "$temp_dir/job.sh" "$temp_dir/safe-extract-tar-zst.py"
(
  cd "$temp_dir"
  shasum -a 256 safe-extract-tar-zst.py >safe-extract.sha256
  shasum -a 256 job.sh safe-extract-tar-zst.py safe-extract.sha256 input.tar.zst input.sha256 >bundle.sha256
)
rm -r -- "$stage" "$source_tree"

if [[ -e "$output_dir" ]]; then
  printf 'output already exists: %s\n' "$output_dir" >&2
  exit 2
fi
mv -- "$temp_dir" "$output_dir"
trap - EXIT
printf 'bundle=%s\nsource_commit=%s\nplan_seed=%s\n' "$output_dir" "$source_commit" "$plan_seed"
