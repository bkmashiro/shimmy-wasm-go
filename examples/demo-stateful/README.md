# Demo: stateful WASM evaluator

This is a deliberately tiny Shimmy-WASM evaluator for live demos.

It mutates a module-global `invocationCount` on every `eval` call. In a normal
warm worker, that state would leak across requests (`1`, then `2`, then `3`).
Shimmy-WASM snapshots the module memory after startup and restores it after each
request, so every request observes `guest_invocation_count: 1`.

Use it via the one-command demo runner:

```bash
scripts/demo-wasm.sh
```

What the demo shows:

1. Build Shimmy.
2. Compile this evaluator to `wasm32-wasip1`.
3. Start `shimmy serve` with `FUNCTION_INTERFACE=wasm`.
4. Send two HTTP grading requests.
5. Assert both responses report `guest_invocation_count == 1`.

That is the visible end-to-end proof: HTTP request → Shimmy → wazero WASM guest
→ response validation → HTTP response, with per-request state reset.
