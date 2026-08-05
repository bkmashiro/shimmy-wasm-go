# Agent Python Ultimate Manual Benchmark

Manual-only benchmark for the exact `agent-python-runtime-numpy-core.wasm`
artifact. It exercises the production `AgentPythonDispatcher`; the HTTP campaign
adds the production `RuntimeHandler` and `CommandHandler` across a real loopback
TCP connection.

## Hard rules

- Never invoke from CI. `scripts/benchmark-agent-python-ultimate.sh` and
  `scripts/benchmark-agent-python-doc.sh` reject common CI environments and
  expose no override.
- Bind every run to the artifact, manifest, input config, executable SHA-256,
  and source commit.
- Treat `snapshot_selected` as observed evidence. A requested `cow` row that
  selects `memcpy` is `unavailable`, never a COW result.
- Raw row JSON is canonical. Summaries are rebuildable with `validate`; no
  outlier is deleted.
- `metadata.complete` means every planned row has a structurally valid terminal
  record; it is not a success verdict. Consumers must inspect per-row status and
  the report's `ok` / `unavailable` / `failed` aggregates. Resume only reuses an
  exact matching row with `ok`, `unavailable`, or `unsupported` status; failed
  or behavior-drifted rows are executed again.
- Remote input and output live under `/tmp/shimmy-agent-python-$SLURM_JOB_ID`.
  A successful result remains there for up to 48 hours until the Mac streams
  and verifies the archive, extracts it, validates the exact-source raw report,
  and only then sends `ACK`; all exit paths remove the exact guarded job
  directory.

## Matrix

`configs/ultimate.json` expands deterministically to 1,401 curated rows across:

- `fresh`, `single-use`, `snapshot/memcpy`, `snapshot/cow`
- cold/warm compilation cache and direct/public-HTTP startup
- 256 B–1,020 KiB inputs and 128 B–900 KiB outputs
- flat, nested, numeric-array, and UTF-8 payloads
- 0/8/32/64/128 MiB prepared arenas
- 0%, 0.01%, 0.1%, 1%, 10%, 50%, and 100% dirty rates
- contiguous, sparse, and fixed-seed random dirtiness
- Python loops, NumPy vector operations, and NumPy matrix multiplication
- pool/prepared capacity 1–4 and concurrency 1–16
- exception, timeout, cancellation, memory growth, oversized payload, and
  post-fault recovery

`--limit` is only for explicit smoke runs. The limit and resulting plan are
stored in the run directory.

### Focused replication plans

Two small plans are reserved for independent Slurm blocks:

- `configs/focused-dirty.json`: 144 matched rows covering three reusable
  lifecycles, 8/32/64/128 MiB arenas, 10/100/1000/5000 dirty basis points,
  and contiguous/sparse/fixed-seed-random page order. Each row records three
  raw requests.
- `configs/focused-lifecycle.json`: eight rows crossing all four lifecycles
  with concurrency 1 and 4. Existing phase events separate startup,
  checkout/wait, execute, restore/close, refill/replacement, and shutdown.

Use a different explicit plan seed for each independent block. The seed is
stored in `plan.json`, determines a stable per-row worker seed, and contributes
to `plan_sha256`. Resume is allowed only when executable, source, artifact,
manifest, config, and plan hashes all match.

## Local gates

```bash
go test ./experiments/agent-python-ultimate -count=1
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest \
  scripts.tests.test_benchmark_agent_python_doc \
  scripts.tests.test_benchmark_agent_python_ultimate_manual
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -o /tmp/agent-python-ultimate ./experiments/agent-python-ultimate
```

A local exact-artifact smoke can use:

```bash
scripts/benchmark-agent-python-ultimate.sh run \
  --config experiments/agent-python-ultimate/configs/smoke.json \
  --artifact build/python-reactor/artifacts/agent-python-runtime-numpy-core.wasm \
  --manifest build/python-reactor/artifacts/manifest.json \
  --output /tmp/agent-python-smoke --limit 4 --max-duration 10m
```

## DoC allocation

Canonical manual request:

```text
partition=a16, node=gpuvm36, 1×nvidia_a16, 6 CPUs, 48 GiB RAM,
60 hours, non-exclusive, export=NIL
```

The GPU is a scheduling requirement and is not used by the benchmark. CPU and
memory claims apply only to the allocated Slurm cgroup on the fixed shared host.
The controller uses `sbcast` after the job reaches `RUNNING`; the job does not
read `/vol/bitbucket` and needs no remote Go, Docker, or sudo.

After committing the exact source, prepare one local bundle per config and
seed. Bundle preparation performs no SSH or Slurm action:

```bash
scripts/prepare-agent-python-doc-bundle.sh \
  /tmp/shimmy-dirty-seed-2026080601 \
  experiments/agent-python-ultimate/configs/focused-dirty.json \
  2026080601
scripts/prepare-agent-python-doc-bundle.sh \
  /tmp/shimmy-lifecycle-seed-2026080602 \
  experiments/agent-python-ultimate/configs/focused-lifecycle.json \
  2026080602
```

`upload` independently verifies the signed source commit, reconstructs the Linux
runner and plan preview from `git archive`, and compares the committed config,
Slurm script, safe extractor, and all bundle hashes before any SSH action.

The manual sequence is `upload -> submit -> stage`; the controller connects to
the `gpucluster2` Slurm submission host by default and reuses a 15-minute SSH
ControlMaster. Result validation, pull, ACK, and controller cleanup remain
separate:

```bash
RUN_ID=agent-python-YYYYMMDDthhmmssz-<8-hex>
scripts/benchmark-agent-python-doc.sh upload "$RUN_ID" /tmp/shimmy-dirty-seed-2026080601
JOB_ID="$(scripts/benchmark-agent-python-doc.sh submit "$RUN_ID")"
scripts/benchmark-agent-python-doc.sh stage "$RUN_ID" "$JOB_ID"
# After RESULT_READY=yes, choose a new local destination. Pull performs bounded
# streaming extraction plus `agent-python-ultimate validate --require-provenance`
# before writing its receipt.
RESULT_DIR="$HOME/shimmy-results/$RUN_ID"
scripts/benchmark-agent-python-doc.sh pull "$JOB_ID" "$RESULT_DIR"
scripts/benchmark-agent-python-doc.sh ack "$JOB_ID" "$RESULT_DIR"
scripts/benchmark-agent-python-doc.sh cleanup-controller "$RUN_ID"
```

Repeat with distinct run IDs and seeds. DoC rows establish the current Agent
Python and Linux lifecycle/dirty-state evidence only; Lambda DBI, Lambda UFFD,
and QEMU remain separate experiment classes.

## Output

Each run contains:

- `metadata.json`: hashes, build info, host, Slurm identity, completion state
- `plan.json`: exact deterministic row list
- `rows/*.input.json`: immutable worker inputs
- `rows/*.json`: raw per-row phases, requests, selected mechanism, faults,
  process `/proc` and cgroup samples
- `checkpoint.jsonl`: fsync-backed completion log
- `report.json`: recomputed campaign/lane summaries
- `report.recomputed.json`: independent `validate` output
- `provenance/input-manifest.json` and `provenance/plan.preview.json`: signed-source
  bundle identity repeated into the result for compute-side and pull-side validation

`pull` runs `agent-python-ultimate validate --output RUN_DIR --require-provenance`
after transport and writes `validation-receipt.json`. `ack` verifies that receipt
against the local result archive and Slurm job ID, then recomputes the remote
archive digest before ACK and cleanup. Compare the two reports before making any
public performance claim.
