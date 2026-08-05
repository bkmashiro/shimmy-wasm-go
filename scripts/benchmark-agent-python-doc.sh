#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATEWAY="${SHIMMY_DOC_GATEWAY:-gpucluster2}"
CONTROL_PATH="${SHIMMY_DOC_CONTROL_PATH:-$HOME/.ssh/cm-%C}"
SSH=(ssh -o BatchMode=yes -o ControlMaster=auto -o ControlPersist=15m -o ControlPath="$CONTROL_PATH" "$GATEWAY")
SCP=(scp -q -o BatchMode=yes -o ControlMaster=auto -o ControlPersist=15m -o ControlPath="$CONTROL_PATH")

for name in CI GITHUB_ACTIONS GITLAB_CI BUILDKITE CIRCLECI JENKINS_URL; do
  value="${!name-}"
  case "$value" in
    ""|0|false|FALSE|no|NO|off|OFF) ;;
    *)
      printf 'agent-python DoC benchmark is manual-only; refusing CI environment (%s)\n' "$name" >&2
      exit 2
      ;;
  esac
done

validate_gateway() {
  local gateway="${1-}"
  if [[ ! "$gateway" =~ ^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$ ]]; then
    printf 'invalid SSH gateway: %q\n' "$gateway" >&2
    return 2
  fi
}
validate_gateway "$GATEWAY"

validate_run_id() {
  local run_id="${1-}"
  if [[ ! "$run_id" =~ ^agent-python-[0-9]{8}t[0-9]{6}z-[a-f0-9]{8}$ ]]; then
    printf 'invalid run id: %q\n' "$run_id" >&2
    return 2
  fi
}

validate_job_id() {
  local job_id="${1-}"
  if [[ ! "$job_id" =~ ^[0-9]+$ ]]; then
    printf 'invalid Slurm job id: %q\n' "$job_id" >&2
    return 2
  fi
}

gateway_root() {
  local run_id="$1"
  validate_run_id "$run_id"
  printf '/tmp/shimmy-agent-python-controller-%s' "$run_id"
}

receive_bounded_stdin() {
  local destination="$1"
  local maximum_bytes="$2"
  local exact_bytes="${3:-0}"
  python3 -c '
import os, pathlib, sys
path = pathlib.Path(sys.argv[1])
maximum = int(sys.argv[2])
expected = int(sys.argv[3])
read_total = 0
try:
    with path.open("xb") as output:
        while True:
            chunk = sys.stdin.buffer.read(min(1048576, maximum - read_total + 1))
            if not chunk:
                break
            read_total += len(chunk)
            if read_total > maximum:
                raise ValueError("stream exceeds byte limit")
            output.write(chunk)
        output.flush()
        os.fsync(output.fileno())
    if read_total <= 0 or (expected and read_total != expected):
        raise ValueError(f"stream size mismatch: received={read_total} expected={expected}")
except BaseException:
    path.unlink(missing_ok=True)
    raise
' "$destination" "$maximum_bytes" "$exact_bytes"
}

render_sbatch() {
  local run_id="$1"
  local job_script="${2:-$(gateway_root "$run_id")/job.sh}"
  validate_run_id "$run_id"
  case "$job_script" in
    /tmp/shimmy-agent-python-controller-"$run_id"/job.sh) ;;
    *) printf 'unsafe remote job script: %q\n' "$job_script" >&2; return 2 ;;
  esac
  printf '%s\n' \
    "sbatch" \
    "--parsable" \
    "--partition=a16" \
    "--nodelist=gpuvm36" \
    "--nodes=1" \
    "--ntasks=1" \
    "--cpus-per-task=6" \
    "--mem=48G" \
    "--gres=gpu:nvidia_a16:1" \
    "--time=2-12:00:00" \
    "--export=NIL" \
    "--chdir=/tmp" \
    "--output=/tmp/shimmy-agent-python-%j-slurm.out" \
    "--job-name=${run_id}" \
    "$job_script"
}

submit_job() {
  local run_id="$1"
  local root
  root="$(gateway_root "$run_id")"
  "${SSH[@]}" bash -s -- "$root" "$run_id" <<'REMOTE'
set -euo pipefail
root="$1"
run_id="$2"
case "$root" in /tmp/shimmy-agent-python-controller-agent-python-*) ;; *) exit 2 ;; esac
[[ "$(cat "$root/.controller-owner")" == "$run_id" ]]
(
  cd "$root"
  sha256sum -c bundle.sha256 >/dev/null
  bash -n job.sh
)
sbatch --parsable \
  --partition=a16 --nodelist=gpuvm36 --nodes=1 --ntasks=1 \
  --cpus-per-task=6 --mem=48G --gres=gpu:nvidia_a16:1 \
  --time=2-12:00:00 --export=NIL --chdir=/tmp \
  --output=/tmp/shimmy-agent-python-%j-slurm.out \
  --job-name="$run_id" "$root/job.sh"
REMOTE
}

upload_bundle() {
  local run_id="$1"
  local bundle_dir="$2"
  validate_run_id "$run_id"
  [[ -d "$bundle_dir" ]] || { printf 'missing bundle directory: %s\n' "$bundle_dir" >&2; return 2; }
  bundle_dir="$(cd "$bundle_dir" && pwd -P)"
  for name in job.sh safe-extract-tar-zst.py safe-extract.sha256 input.tar.zst input.sha256 bundle.sha256; do
    [[ -s "$bundle_dir/$name" ]] || { printf 'missing bundle file: %s\n' "$bundle_dir/$name" >&2; return 2; }
  done
  (
    cd "$bundle_dir"
    shasum -a 256 -c bundle.sha256 >/dev/null
  )
  "$ROOT_DIR/experiments/agent-python-ultimate/scripts/verify_doc_bundle.py" \
    "$bundle_dir" "$ROOT_DIR"
  local root
  root="$(gateway_root "$run_id")"
  "${SSH[@]}" bash -s -- "$root" "$run_id" <<'REMOTE'
set -euo pipefail
root="$1"
run_id="$2"
case "$root" in /tmp/shimmy-agent-python-controller-agent-python-*) ;; *) exit 2 ;; esac
if ! mkdir -m 0700 -- "$root"; then
  printf 'controller root already exists: %s\n' "$root" >&2
  exit 3
fi
printf '%s\n' "$run_id" >"$root/.controller-owner"
REMOTE
  "${SCP[@]}" "$bundle_dir/job.sh" "$bundle_dir/safe-extract-tar-zst.py" "$bundle_dir/safe-extract.sha256" \
    "$bundle_dir/input.tar.zst" "$bundle_dir/input.sha256" "$bundle_dir/bundle.sha256" "$GATEWAY:$root/"
  "${SSH[@]}" bash -s -- "$root" "$run_id" <<'REMOTE'
set -euo pipefail
root="$1"
run_id="$2"
[[ "$(cat "$root/.controller-owner")" == "$run_id" ]]
chmod 0700 "$root/job.sh" "$root/safe-extract-tar-zst.py"
chmod 0600 "$root/safe-extract.sha256" "$root/input.tar.zst" "$root/input.sha256" "$root/bundle.sha256"
bash -n "$root/job.sh"
cd "$root"
sha256sum -c bundle.sha256
REMOTE
}

stage_job() {
  local run_id="$1"
  local job_id="$2"
  validate_run_id "$run_id"
  validate_job_id "$job_id"
  local root
  root="$(gateway_root "$run_id")"
  "${SSH[@]}" bash -s -- "$root" "$job_id" "$run_id" <<'REMOTE'
set -euo pipefail
root="$1"
job_id="$2"
run_id="$3"
case "$root" in /tmp/shimmy-agent-python-controller-agent-python-*) ;; *) exit 2 ;; esac
[[ "$(cat "$root/.controller-owner")" == "$run_id" ]]
(cd "$root" && sha256sum -c bundle.sha256 >/dev/null)
for _ in $(seq 1 600); do
  state="$(squeue -h -j "$job_id" -o '%T')"
  case "$state" in
    RUNNING) break ;;
    PENDING|CONFIGURING) sleep 1 ;;
    *) printf 'job %s entered state %s before stage\n' "$job_id" "$state" >&2; exit 3 ;;
  esac
done
[[ "${state-}" == RUNNING ]]
sleep 5
for name in input.tar.zst input.sha256 safe-extract-tar-zst.py safe-extract.sha256; do
  broadcast_output="$(sbcast -v --force --jobid="$job_id.batch" "$root/$name" "/tmp/shimmy-agent-python-$job_id/$name" 2>&1)"
  printf '%s\n' "$broadcast_output"
  case "$broadcast_output" in
    *"jobid      = $job_id.batch"*) ;;
    *) printf 'sbcast did not confirm the batch step credential for %s\n' "$name" >&2; exit 4 ;;
  esac
done
sleep 5
for name in input.tar.zst input.sha256 safe-extract-tar-zst.py safe-extract.sha256; do
  srun --jobid="$job_id" --overlap -N1 -n1 test -s "/tmp/shimmy-agent-python-$job_id/$name"
done
REMOTE
}

job_status() {
  local job_id="$1"
  validate_job_id "$job_id"
  "${SSH[@]}" bash -s -- "$job_id" <<'REMOTE'
set -euo pipefail
job_id="$1"
squeue -j "$job_id" -o '%.18i %.12T %.20S %.20e %.8M %.9l %.6D %R'
sacct -j "$job_id" --starttime now-7days -X -n -P -o JobID,State,Elapsed,Timelimit,NodeList,ExitCode 2>/dev/null || true
if [[ "$(squeue -h -j "$job_id" -o '%T')" == RUNNING ]]; then
  if srun --jobid="$job_id" --overlap -N1 -n1 test -f "/tmp/shimmy-agent-python-$job_id/RESULT.READY" 2>/dev/null; then
    printf 'RESULT_READY=yes\n'
  else
    printf 'RESULT_READY=no\n'
  fi
fi
REMOTE
}

pull_result() {
  local job_id="$1"
  local local_dir="$2"
  validate_job_id "$job_id"
  if [[ -e "$local_dir" ]]; then
    printf 'local result destination already exists: %s\n' "$local_dir" >&2
    return 2
  fi
  mkdir -p -m 0700 -- "$local_dir"
  local_dir="$(cd "$local_dir" && pwd -P)"
  local temp_dir="$local_dir/.pull-$job_id"
  local extract_dir="$temp_dir/extract"
  cleanup_pull() {
    rm -rf -- "$temp_dir"
    rmdir "$local_dir" 2>/dev/null || true
  }
  trap cleanup_pull EXIT
  mkdir -m 0700 -- "$temp_dir"
  "${SSH[@]}" srun --jobid="$job_id" --overlap -N1 -n1 \
    cat "/tmp/shimmy-agent-python-$job_id/result.transport.json" \
    | receive_bounded_stdin "$temp_dir/result.transport.json" 65536
  local transport_fields
  transport_fields="$(python3 - "$job_id" "$temp_dir/result.transport.json" <<'PY'
import json
import pathlib
import re
import sys

job_id, path = sys.argv[1:]
transport = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
if set(transport) != {"schema", "job_id", "bytes", "sha256"}:
    raise SystemExit("unexpected result transport fields")
if transport.get("schema") != "shimmy-agent-python-doc-result-transport/v1" or str(transport.get("job_id")) != job_id:
    raise SystemExit("result transport identity mismatch")
size = transport.get("bytes")
digest = transport.get("sha256")
if not isinstance(size, int) or not 0 < size <= 536870912:
    raise SystemExit("result compressed size is outside 1..512 MiB")
if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
    raise SystemExit("invalid result transport hash")
print(f"{size}\t{digest}")
PY
)"
  if [[ "$transport_fields" != *$'\t'* ]]; then
    printf 'transport identity did not contain two tab-separated fields\n' >&2
    return 2
  fi
  local expected_bytes="${transport_fields%%$'\t'*}"
  local expected_hash="${transport_fields#*$'\t'}"
  if [[ "$expected_hash" == *$'\t'* ]]; then
    printf 'transport identity contained extra fields\n' >&2
    return 2
  fi
  "${SSH[@]}" srun --jobid="$job_id" --overlap -N1 -n1 \
    cat "/tmp/shimmy-agent-python-$job_id/result.sha256" \
    | receive_bounded_stdin "$temp_dir/result.sha256" 256
  checksum_line="$(cat "$temp_dir/result.sha256")"
  if [[ "$checksum_line" != "$expected_hash  result.tar.zst" ]]; then
    printf 'result checksum and transport manifest disagree\n' >&2
    return 2
  fi
  "${SSH[@]}" srun --jobid="$job_id" --overlap -N1 -n1 \
    cat "/tmp/shimmy-agent-python-$job_id/result.tar.zst" \
    | receive_bounded_stdin "$temp_dir/result.tar.zst" 536870912 "$expected_bytes"
  (
    cd "$temp_dir"
    shasum -a 256 -c result.sha256
  )
  "$ROOT_DIR/experiments/agent-python-ultimate/scripts/safe_extract_tar_zst.py" \
    "$temp_dir/result.tar.zst" "$extract_dir" \
    --max-compressed-bytes 536870912 --max-members 200000 \
    --max-total-bytes 4294967296 --max-file-bytes 2147483648 \
    --required-root output
  result_source="$(python3 - "$extract_dir/output/run/metadata.json" <<'PY'
import json
import pathlib
import re
import sys

source = str(json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")).get("source_commit", ""))
if not re.fullmatch(r"[0-9a-f]{40}", source):
    raise SystemExit("invalid result source commit")
print(source)
PY
)"
  current_source="$(git -C "$ROOT_DIR" rev-parse HEAD)"
  if [[ "$current_source" != "$result_source" ]]; then
    printf 'validator source mismatch: result=%s current=%s\n' "$result_source" "$current_source" >&2
    return 2
  fi
  git -C "$ROOT_DIR" verify-commit "$current_source" >/dev/null
  validator_source="$temp_dir/validator-source"
  mkdir -m 0700 -- "$validator_source"
  git -C "$ROOT_DIR" archive --format=tar "$current_source" | tar -xf - -C "$validator_source"
  (
    cd "$validator_source"
    go run ./experiments/agent-python-ultimate validate \
      --output "$extract_dir/output/run" --require-provenance
  )
  python3 - "$job_id" "$temp_dir/result.tar.zst" "$extract_dir/output/run" "$temp_dir/validation-receipt.json" <<'PY'
import hashlib
import json
import pathlib
import sys
from datetime import datetime, timezone

job_id, archive_path, run_dir, receipt_path = sys.argv[1:]
archive = pathlib.Path(archive_path)
run = pathlib.Path(run_dir)
metadata = json.loads((run / "metadata.json").read_text(encoding="utf-8"))
if str(metadata.get("slurm_job_id", "")) != job_id:
    raise SystemExit("result metadata Slurm job ID mismatch")
digest = hashlib.sha256()
with archive.open("rb") as handle:
    for chunk in iter(lambda: handle.read(1048576), b""):
        digest.update(chunk)
receipt = {
    "schema": "shimmy-agent-python-doc-validation/v1",
    "job_id": job_id,
    "result_sha256": digest.hexdigest(),
    "source_commit": metadata.get("source_commit"),
    "plan_sha256": metadata.get("plan_sha256"),
    "validated_utc": datetime.now(timezone.utc).isoformat(),
}
pathlib.Path(receipt_path).write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
PY
  mv -- "$temp_dir/result.tar.zst" "$local_dir/result.tar.zst"
  mv -- "$temp_dir/result.sha256" "$local_dir/result.sha256"
  mv -- "$temp_dir/result.transport.json" "$local_dir/result.transport.json"
  mv -- "$extract_dir/output" "$local_dir/output"
  mv -- "$temp_dir/validation-receipt.json" "$local_dir/validation-receipt.json"
  rm -rf -- "$temp_dir"
  trap - EXIT
}

ack_result() {
  local job_id="$1"
  local local_dir="$2"
  validate_job_id "$job_id"
  [[ -d "$local_dir" ]] || { printf 'missing validated result directory: %s\n' "$local_dir" >&2; return 2; }
  local_dir="$(cd "$local_dir" && pwd -P)"
  expected_hash="$(python3 - "$job_id" "$local_dir/validation-receipt.json" "$local_dir/result.tar.zst" <<'PY'
import hashlib
import json
import pathlib
import sys

job_id, receipt_path, archive_path = sys.argv[1:]
receipt = json.loads(pathlib.Path(receipt_path).read_text(encoding="utf-8"))
archive = pathlib.Path(archive_path)
if receipt.get("schema") != "shimmy-agent-python-doc-validation/v1":
    raise SystemExit("invalid validation receipt schema")
if str(receipt.get("job_id", "")) != job_id:
    raise SystemExit("validation receipt job ID mismatch")
digest = hashlib.sha256()
with archive.open("rb") as handle:
    for chunk in iter(lambda: handle.read(1048576), b""):
        digest.update(chunk)
if receipt.get("result_sha256") != digest.hexdigest():
    raise SystemExit("validation receipt result hash mismatch")
print(digest.hexdigest())
PY
)"
  "${SSH[@]}" srun --jobid="$job_id" --overlap -N1 -n1 \
    bash -s -- "/tmp/shimmy-agent-python-$job_id" "$expected_hash" <<'REMOTE'
set -euo pipefail
root="$1"
expected_hash="$2"
case "$root" in /tmp/shimmy-agent-python-[0-9]*) ;; *) exit 2 ;; esac
[[ "$expected_hash" =~ ^[0-9a-f]{64}$ ]]
[[ -f "$root/RESULT.READY" ]]
actual_hash="$(sha256sum "$root/result.tar.zst" | cut -d' ' -f1)"
[[ "$actual_hash" == "$expected_hash" ]]
touch "$root/ACK"
REMOTE
}

cleanup_controller() {
  local run_id="$1"
  local root
  root="$(gateway_root "$run_id")"
  "${SSH[@]}" bash -s -- "$root" "$run_id" <<'REMOTE'
set -euo pipefail
root="$1"
run_id="$2"
case "$root" in /tmp/shimmy-agent-python-controller-agent-python-*) ;; *) exit 2 ;; esac
[[ "$(cat "$root/.controller-owner")" == "$run_id" ]]
rm -rf -- "$root"
REMOTE
}

usage() {
  printf 'usage: %s COMMAND ...\ncommands: validate-run-id RUN_ID | render-sbatch RUN_ID | upload RUN_ID BUNDLE_DIR | submit RUN_ID | stage RUN_ID JOB_ID | status JOB_ID | pull JOB_ID NEW_LOCAL_DIR | ack JOB_ID VALIDATED_LOCAL_DIR | cleanup-controller RUN_ID\n' "$0" >&2
  exit 2
}

command_name="${1-}"
case "$command_name" in
  validate-run-id) [[ $# -eq 2 ]] || usage; validate_run_id "$2" ;;
  render-sbatch) [[ $# -eq 2 ]] || usage; render_sbatch "$2" ;;
  upload) [[ $# -eq 3 ]] || usage; upload_bundle "$2" "$3" ;;
  submit) [[ $# -eq 2 ]] || usage; submit_job "$2" ;;
  stage) [[ $# -eq 3 ]] || usage; stage_job "$2" "$3" ;;
  status) [[ $# -eq 2 ]] || usage; job_status "$2" ;;
  pull) [[ $# -eq 3 ]] || usage; pull_result "$2" "$3" ;;
  ack) [[ $# -eq 3 ]] || usage; ack_result "$2" "$3" ;;
  cleanup-controller) [[ $# -eq 2 ]] || usage; cleanup_controller "$2" ;;
  *) usage ;;
esac
