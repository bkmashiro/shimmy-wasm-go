"""
eval-scipy: statistical grading via SciPy.

Route: Pyodide/Node.js runner, not CPython-WASI/reactor-python.
SciPy depends on compiled extension modules that are not available in the
current CPython-WASI sandbox, while Pyodide ships SciPy as WebAssembly wheels.

Supported modes
---------------
numeric  – scalar numeric equality using math.isclose
ttest    – one-sample t-test using scipy.stats.ttest_1samp

Example t-test request:
{
  "response": "",
  "answer": "5.0",
  "params": {
    "test": "ttest",
    "samples": [4.9, 5.1, 5.0, 5.2, 4.8],
    "alpha": 0.05
  }
}
"""

from scipy import stats
import math


def _evaluate(response, answer, params=None):
    params = params or {}
    mode = params.get("test", "numeric")

    try:
        if mode == "ttest":
            return _ttest(response, answer, params)
        return _numeric(response, answer, params)
    except Exception as exc:
        return {
            "is_correct": False,
            "feedback": f"Evaluation error: {exc}",
        }


def _numeric(response, answer, params):
    tolerance = float(params.get("tolerance", 1e-6))
    r = float(response)
    a = float(answer)
    correct = math.isclose(r, a, abs_tol=tolerance, rel_tol=0.0)
    return {
        "is_correct": correct,
        "feedback": (
            f"Correct: {r} ~= {a}."
            if correct
            else f"Incorrect: got {r}, expected {a}, tolerance={tolerance}."
        ),
        "absolute_error": abs(r - a),
    }


def _ttest(response, answer, params):
    alpha = float(params.get("alpha", 0.05))
    samples = params.get("samples")
    if samples is None:
        return {
            "is_correct": False,
            "feedback": "t-test mode requires params.samples.",
        }

    values = [float(x) for x in samples]
    if len(values) < 2:
        return {
            "is_correct": False,
            "feedback": "t-test requires at least two samples.",
        }

    popmean = float(answer)
    t_stat, p_value = stats.ttest_1samp(values, popmean)
    sample_mean = sum(values) / len(values)
    correct = bool(p_value > alpha)

    return {
        "is_correct": correct,
        "feedback": (
            f"Sample mean {sample_mean:.4g} is statistically consistent with "
            f"{popmean} (p={p_value:.4g} > alpha={alpha})."
            if correct
            else f"Sample mean {sample_mean:.4g} differs from {popmean} "
                 f"(p={p_value:.4g} <= alpha={alpha})."
        ),
        "sample_mean": sample_mean,
        "t_statistic": float(t_stat),
        "p_value": float(p_value),
        "n_samples": len(values),
    }


def _preview(response, answer, params=None):
    params = params or {}
    if params.get("test") == "ttest":
        samples = params.get("samples", [])
        return {"preview": f"One-sample t-test with {len(samples)} samples."}
    return {"preview": f"SciPy numeric comparison against {answer}."}

def dispatch(method, payload):
    """Evaluator-owned method routing for the Python Reactor ABI."""
    if not isinstance(payload, dict):
        raise TypeError("payload must be a dict")
    if method == "eval":
        return _evaluate(payload.get("response"), payload.get("answer"), payload.get("params", {}))
    if method == "preview":
        return _preview(payload.get("response"), payload.get("answer"), payload.get("params", {}))
    raise LookupError("unsupported method: " + str(method))
