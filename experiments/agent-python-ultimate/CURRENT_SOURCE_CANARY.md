# Current-source Python Reactor canary

This is a bounded confirmation run for the current Shimmy source and the pinned
Python Reactor artifact. It does not replace or silently mix with the complete
campaign measured at `0024edd`.

## Artifact decision

The pinned `agent-python-runtime-numpy-core.wasm` does not need rebuilding for
this canary. Its low-level Reactor ABI and manifest still validate against the
actual module. Evaluator bundles that only define `evaluation_function` or
`preview_function` must be regenerated so that the artifact owns
`dispatch(method, payload)`.

## Matrix

The `current-canary` campaign has exactly ten rows:

- fresh, single-use, snapshot/memcpy and snapshot/COW at concurrency 1;
- snapshot/memcpy and snapshot/COW at concurrency 4;
- 32 MiB at 1% dirty pages and 64 MiB at 10% dirty pages;
- direct dispatch and two public HTTP rows;
- five steady requests per ordinary row and twelve requests per burst row.

Every row preserves startup phases, request samples, snapshot selection,
prepared hit/miss/refill counters, RSS/PSS and exact source/artifact identity.
No outlier is deleted.

## Prepare locally

The worktree must be committed, clean outside `.hermes/`, and signed.

```bash
output="$HOME/.cache/shimmy-canary/$(date -u +%Y%m%dT%H%M%SZ)"
scripts/prepare-current-source-canary.sh "$output"
```

The wrapper reuses `prepare-agent-python-doc-bundle.sh`, which builds a static
Linux/amd64 runner from the signed commit, copies the pinned artifact and
manifest, materializes the ten-row plan, and writes checksums.

## ICL control-plane flow

Use one SSH ControlMaster. Start with `shell2` for identity, then use the
configured `gpucluster2` scheduler host. Login hosts are control planes only.

```bash
run_id="python-reactor-canary-$(date -u +%Y%m%dT%H%M%SZ)"
scripts/benchmark-agent-python-doc.sh upload "$run_id" "$output"
scripts/benchmark-agent-python-doc.sh render-sbatch "$run_id"
# Inspect live partition/TRES state and run sbatch --test-only before submit.
# Then use submit, stage, status (at intervals >= 1 minute), pull and ack.
```

Do not submit a second job after an ambiguous timeout until `squeue`/`sacct` and
the run-specific controller path prove that the first submission did not land.

## Promotion rule

This is a current-source confirmation, not a new ranking. Compare only against
the same ICL host and the exact matching lifecycle/workload rows. Escalate to a
full rerun only if correctness/reset evidence fails, snapshot selection changes,
or latency/memory moves outside the historical same-host run-to-run envelope.
