# python-reactor-check

Optional advisory checker for evaluator owners preparing a Python Reactor
artifact. It is not imported by Shimmy and never runs in the production request
path.

```bash
python3 tools/python-reactor-check/python_reactor_check.py \
  --source /path/to/evaluator \
  --json
```

It warns about source-level operations whose WASM behavior may differ from a
native interpreter, including subprocesses, network clients, filesystem access,
threads/processes/signals, and bundled native extensions. Static analysis is an
over-approximation: warnings do not decide whether an evaluator is suitable and
do not change the successful exit status.

A caller may explicitly provide a packaging command and expected artifact:

```bash
python3 tools/python-reactor-check/python_reactor_check.py \
  --source evaluator/ \
  --build-command 'python3 package.py --out build/evaluator.bundle.py' \
  --artifact build/evaluator.bundle.py
```

Only objective failures are errors (exit status 1): invalid input paths, failed
explicit build commands, or missing expected artifacts. The tool does not infer
a build system, rewrite evaluator code, or add compatibility shims.

Tests:

```bash
python3 -m unittest tools/python-reactor-check/test_python_reactor_check.py -v
```
