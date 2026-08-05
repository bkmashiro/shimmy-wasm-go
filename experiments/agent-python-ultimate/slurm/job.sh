#!/usr/bin/env bash
set -euo pipefail
umask 077
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

: "${SLURM_JOB_ID:?SLURM_JOB_ID is required}"
case "$SLURM_JOB_ID" in
  *[!0-9]*)
    printf 'invalid SLURM_JOB_ID: %q\n' "$SLURM_JOB_ID" >&2
    exit 2
    ;;
esac

run_root="/tmp/shimmy-agent-python-${SLURM_JOB_ID}"
case "$run_root" in
  /tmp/shimmy-agent-python-[0-9]*) ;;
  *)
    printf 'unsafe run root: %q\n' "$run_root" >&2
    exit 2
    ;;
esac

run_root_created=0
owner_marker="$run_root/.shimmy-owned-run-root"
cleanup_run_root() {
  if (( run_root_created == 0 )); then
    return 0
  fi
  case "$run_root" in
    /tmp/shimmy-agent-python-[0-9]*) ;;
    *) printf 'refusing cleanup of unsafe path: %q\n' "$run_root" >&2; return 2 ;;
  esac
  owner="$(cat "$owner_marker" 2>/dev/null || true)"
  if [[ "$owner" != "$SLURM_JOB_ID" ]]; then
    printf 'refusing cleanup without matching ownership marker: %q\n' "$run_root" >&2
    return 2
  fi
  rm -rf -- "$run_root"
}
trap cleanup_run_root EXIT

if ! mkdir -m 0700 -- "$run_root"; then
  printf 'run root already exists; refusing to reuse: %q\n' "$run_root" >&2
  exit 2
fi
run_root_created=1
printf '%s\n' "$SLURM_JOB_ID" >"$owner_marker"
input_archive="$run_root/input.tar.zst"
input_checksum="$run_root/input.sha256"
safe_extractor="$run_root/safe-extract-tar-zst.py"
safe_extractor_checksum="$run_root/safe-extract.sha256"
input_dir="$run_root/input"
output_dir="$run_root/output"
result_archive="$run_root/result.tar.zst"
result_checksum="$run_root/result.sha256"

wait_for_file() {
  local path="$1"
  local timeout_seconds="$2"
  local waited=0
  while [[ ! -f "$path" ]]; do
    if (( waited >= timeout_seconds )); then
      printf 'timed out waiting for %s\n' "$path" >&2
      return 124
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

# The gateway waits for RUNNING state, then sbcast writes these four files.
wait_for_file "$input_archive" 1800
wait_for_file "$input_checksum" 1800
wait_for_file "$safe_extractor" 1800
wait_for_file "$safe_extractor_checksum" 1800

archive_bytes="$(stat -c '%s' "$input_archive")"
if (( archive_bytes <= 0 || archive_bytes > 268435456 )); then
  printf 'input bundle size %s is outside 1..256 MiB\n' "$archive_bytes" >&2
  exit 2
fi

checksum_line_count="$(wc -l <"$input_checksum" | tr -d '[:space:]')"
checksum_hash=""
checksum_name=""
checksum_extra=""
read -r checksum_hash checksum_name checksum_extra <"$input_checksum" || true
if [[ "$checksum_line_count" != "1" || ! "$checksum_hash" =~ ^[0-9a-f]{64}$ || "$checksum_name" != "input.tar.zst" || -n "$checksum_extra" ]]; then
  printf 'input checksum file must contain exactly one lowercase SHA-256 entry for input.tar.zst\n' >&2
  exit 2
fi
printf '%s  input.tar.zst\n' "$checksum_hash" >"$run_root/input.sha256.canonical"
mv -f -- "$run_root/input.sha256.canonical" "$input_checksum"

(
  cd "$run_root"
  sha256sum -c "$(basename "$input_checksum")"
)

extractor_line_count="$(wc -l <"$safe_extractor_checksum" | tr -d '[:space:]')"
extractor_hash=""
extractor_name=""
extractor_extra=""
read -r extractor_hash extractor_name extractor_extra <"$safe_extractor_checksum" || true
if [[ "$extractor_line_count" != "1" || ! "$extractor_hash" =~ ^[0-9a-f]{64}$ || "$extractor_name" != "safe-extract-tar-zst.py" || -n "$extractor_extra" ]]; then
  printf 'extractor checksum file must contain exactly one lowercase SHA-256 entry\n' >&2
  exit 2
fi
(
  cd "$run_root"
  sha256sum -c "$(basename "$safe_extractor_checksum")"
)
chmod 0700 "$safe_extractor"

free_kib="$(df -Pk /tmp | sed -n '2{s/[[:space:]][[:space:]]*/ /g;p;}' | cut -d' ' -f4)"
if [[ -z "$free_kib" ]] || (( free_kib < 4194304 )); then
  printf 'compute-node /tmp has less than 4 GiB free: %s KiB\n' "${free_kib:-unknown}" >&2
  exit 2
fi

mkdir -m 0700 -- "$output_dir"
"$safe_extractor" "$input_archive" "$input_dir" \
  --max-compressed-bytes 268435456 --max-members 8 \
  --max-total-bytes 1073741824 --max-file-bytes 536870912 \
  --allowed-path agent-python-ultimate \
  --allowed-path ultimate.json \
  --allowed-path agent-python-runtime-numpy-core.wasm \
  --allowed-path manifest.json \
  --allowed-path plan-seed.txt \
  --allowed-path plan.preview.json \
  --allowed-path input-manifest.json \
  --allowed-path input-files.sha256

(
  cd "$input_dir"
  sha256sum -c input-files.sha256
)
python3 - "$input_dir" <<'PY'
import hashlib
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
manifest = json.loads((root / "input-manifest.json").read_text(encoding="utf-8"))
expected = {
    "agent-python-ultimate",
    "ultimate.json",
    "agent-python-runtime-numpy-core.wasm",
    "manifest.json",
    "plan-seed.txt",
    "plan.preview.json",
}
if set(manifest) != {"schema", "source_commit", "config_path", "plan_seed", "files"}:
    raise SystemExit("unexpected input manifest fields")
if manifest.get("schema") != "shimmy-agent-python-doc-input/v1":
    raise SystemExit("unexpected input manifest schema")
if not re.fullmatch(r"[0-9a-f]{40}", str(manifest.get("source_commit", ""))):
    raise SystemExit("invalid input manifest source commit")
config_path = pathlib.PurePosixPath(str(manifest.get("config_path", "")))
if config_path.is_absolute() or not config_path.parts or ".." in config_path.parts or str(config_path) != str(manifest.get("config_path", "")):
    raise SystemExit("invalid input manifest config path")
files = manifest.get("files")
if not isinstance(files, dict) or set(files) != expected:
    raise SystemExit("input manifest file set mismatch")
seed = (root / "plan-seed.txt").read_text(encoding="utf-8").strip()
if manifest.get("plan_seed") != int(seed):
    raise SystemExit("input manifest plan seed mismatch")
for name, record in files.items():
    path = root / name
    raw = path.read_bytes()
    if record != {"bytes": len(raw), "sha256": hashlib.sha256(raw).hexdigest()}:
        raise SystemExit(f"input manifest mismatch for {name}")
PY

# The safe extractor deliberately creates regular files with private 0600
# permissions. Restore execute permission only after the signed manifest and
# every extracted byte have been verified.
chmod 0700 "$input_dir/agent-python-ultimate"

plan_seed_args=()
IFS= read -r plan_seed <"$input_dir/plan-seed.txt"
if [[ ! "$plan_seed" =~ ^[1-9][0-9]*$ ]]; then
  printf 'invalid plan seed: %q\n' "$plan_seed" >&2
  exit 2
fi
plan_seed_args=(--seed "$plan_seed")
"$input_dir/agent-python-ultimate" plan \
  --config "$input_dir/ultimate.json" \
  --seed "$plan_seed" \
  --output "$run_root/plan.executed.json"
cmp -- "$input_dir/plan.preview.json" "$run_root/plan.executed.json"

{
  printf 'captured_at_utc='; date -u +%FT%TZ
  printf 'hostname='; hostname
  printf 'uname='; uname -srmo
  printf 'page_size='; getconf PAGESIZE
  printf 'slurm_job_id=%s\n' "$SLURM_JOB_ID"
  printf 'slurm_job_cpus_per_node=%s\n' "${SLURM_JOB_CPUS_PER_NODE-}"
  printf 'slurm_job_gpus=%s\n' "${SLURM_JOB_GPUS-}"
  printf 'plan_seed=%s\n' "$plan_seed"
  printf 'cpuset='; python3 -c 'import os; print(",".join(map(str, sorted(os.sched_getaffinity(0)))))'
  printf '%s\n' '--- lscpu ---'
  lscpu
  printf '%s\n' '--- df /tmp ---'
  df -h /tmp
  printf '%s\n' '--- cgroup ---'
  cat /proc/self/cgroup
} >"$output_dir/environment.txt" 2>"$output_dir/environment.stderr"

benchmark_rc=0
"$input_dir/agent-python-ultimate" run \
  --config "$input_dir/ultimate.json" \
  --artifact "$input_dir/agent-python-runtime-numpy-core.wasm" \
  --manifest "$input_dir/manifest.json" \
  --output "$output_dir/run" \
  "${plan_seed_args[@]}" \
  >"$output_dir/benchmark.stdout" \
  2>"$output_dir/benchmark.stderr" || benchmark_rc=$?
printf '%s\n' "$benchmark_rc" >"$output_dir/benchmark.exit-code"

if [[ -f "$output_dir/run/metadata.json" && -f "$output_dir/run/plan.json" ]]; then
  mkdir -m 0700 -- "$output_dir/run/provenance"
  cp -- "$input_dir/input-manifest.json" "$output_dir/run/provenance/input-manifest.json"
  cp -- "$input_dir/plan.preview.json" "$output_dir/run/provenance/plan.preview.json"
  python3 - "$input_dir" "$output_dir/run" "$SLURM_JOB_ID" <<'PY'
import hashlib
import json
import pathlib
import sys

inputs = pathlib.Path(sys.argv[1])
run = pathlib.Path(sys.argv[2])
job_id = sys.argv[3]
bundle = json.loads((inputs / "input-manifest.json").read_text(encoding="utf-8"))
metadata = json.loads((run / "metadata.json").read_text(encoding="utf-8"))
plan = json.loads((run / "plan.json").read_text(encoding="utf-8"))
preview = json.loads((inputs / "plan.preview.json").read_text(encoding="utf-8"))
if plan != preview:
    raise SystemExit("executed plan differs from bundle preview")
plan_raw = json.dumps(plan, separators=(",", ":"), ensure_ascii=False).encode()
expected = {
    "source_commit": bundle["source_commit"],
    "plan_sha256": hashlib.sha256(plan_raw).hexdigest(),
    "executable_sha256": bundle["files"]["agent-python-ultimate"]["sha256"],
    "artifact_sha256": bundle["files"]["agent-python-runtime-numpy-core.wasm"]["sha256"],
    "manifest_sha256": bundle["files"]["manifest.json"]["sha256"],
    "config_sha256": bundle["files"]["ultimate.json"]["sha256"],
    "slurm_job_id": job_id,
}
for key, value in expected.items():
    if str(metadata.get(key, "")) != value:
        raise SystemExit(f"run metadata mismatch for {key}")
PY
  if (( benchmark_rc == 0 )); then
    "$input_dir/agent-python-ultimate" validate \
      --output "$output_dir/run" --require-provenance
  fi
elif (( benchmark_rc == 0 )); then
  printf 'successful runner exit without metadata/plan\n' >&2
  exit 2
fi

(
  cd "$run_root"
  tar -cf - output | zstd -q -T0 -19 -o "$result_archive"
  sha256sum "$(basename "$result_archive")" >"$result_checksum"
)
result_bytes="$(stat -c '%s' "$result_archive")"
if (( result_bytes <= 0 || result_bytes > 536870912 )); then
  printf 'result bundle size %s is outside 1..512 MiB\n' "$result_bytes" >&2
  exit 2
fi
result_hash="$(cut -d' ' -f1 <"$result_checksum")"
python3 - "$run_root/result.transport.json" "$SLURM_JOB_ID" "$result_bytes" "$result_hash" <<'PY'
import json
import pathlib
import sys

path, job_id, size, digest = sys.argv[1:]
pathlib.Path(path).write_text(json.dumps({
    "schema": "shimmy-agent-python-doc-result-transport/v1",
    "job_id": job_id,
    "bytes": int(size),
    "sha256": digest,
}, indent=2) + "\n", encoding="utf-8")
PY
: >"$run_root/RESULT.READY"

ack_rc=0
wait_for_file "$run_root/ACK" 172800 || ack_rc=$?

if (( ack_rc != 0 )); then
  exit "$ack_rc"
fi
exit "$benchmark_rc"
