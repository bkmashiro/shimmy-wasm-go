"""Compatibility adapter for Lambda Feedback evaluator packages.

This module is intentionally minimal and backend-agnostic. It helps normalize
the tiny differences between evaluator entrypoint styles and return object types.
"""

from __future__ import annotations

from dataclasses import asdict, is_dataclass
import inspect
import importlib


def load_entrypoint(spec: str):
    """Load a callable from a package-style entrypoint string.

    Accepted format:
        "evaluation_function.evaluation:evaluation_function"
    """

    if not isinstance(spec, str) or ":" not in spec:
        raise ValueError("Entrypoint must use module:function format")

    module_name, symbol_name = spec.rsplit(":", 1)
    module = importlib.import_module(module_name)
    return getattr(module, symbol_name)


def _preview_accepts_two_args(fn) -> bool:
    """Best-effort detection for preview functions with two positional args."""

    try:
        sig = inspect.signature(fn)
    except (TypeError, ValueError):
        return False

    positional = [
        p
        for p in sig.parameters.values()
        if p.kind in (inspect.Parameter.POSITIONAL_ONLY, inspect.Parameter.POSITIONAL_OR_KEYWORD)
    ]
    has_varargs = any(
        p.kind == inspect.Parameter.VAR_POSITIONAL for p in sig.parameters.values()
    )
    # Preview in fixtures can be two args:
    #    preview(response, params)
    # and in some cases three args:
    #    preview(response, answer, params)
    return (not has_varargs) and len(positional) == 2


def call_function(fn, method: str, response, answer=None, params=None):
    """Call evaluation/preview handlers with backend-compatible arity."""

    if params is None:
        params = {}

    if method == "preview":
        if _preview_accepts_two_args(fn):
            return fn(response, params)
        return fn(response, answer, params)

    if method == "eval":
        return fn(response, answer, params)

    return fn(response, answer, params)


def normalize_result(value):
    """Normalize toolkit objects to plain dictionaries.

    Supported conversions, in order:
    - dict passthrough
    - Pydantic model style: model_dump()
    - dict()-style objects
    - dataclasses
    - attrs-like / public-field objects (via vars()/__dict__ or __slots__)

    Nested toolkit objects are recursively converted so the result can be JSON
    encoded by Pyodide/reactor/stdio runners.
    """

    normalized = _normalize_jsonish(value)
    if isinstance(normalized, dict):
        return normalized
    return {"value": normalized}


def _normalize_jsonish(value):
    if value is None or isinstance(value, (str, int, float, bool)):
        return value

    if isinstance(value, dict):
        return {k: _normalize_jsonish(v) for k, v in value.items()}

    if isinstance(value, (list, tuple)):
        return [_normalize_jsonish(v) for v in value]

    if hasattr(value, "model_dump") and callable(getattr(value, "model_dump")):
        return _normalize_jsonish(value.model_dump())

    if hasattr(value, "dict") and callable(getattr(value, "dict")):
        out = value.dict()
        if isinstance(out, dict):
            return _normalize_jsonish(out)

    # NumPy scalar values (for example np.bool_ from np.allclose) expose
    # item() but are not instances of Python's built-in scalar types.
    item = getattr(value, "item", None)
    if callable(item):
        try:
            scalar = item()
        except (TypeError, ValueError):
            scalar = value
        if scalar is not value and isinstance(scalar, (str, int, float, bool)):
            return scalar

    if is_dataclass(value) and not isinstance(value, type):
        return _normalize_jsonish(asdict(value))

    fields = vars(value) if hasattr(value, "__dict__") else None
    if isinstance(fields, dict):
        return {
            k: _normalize_jsonish(v)
            for k, v in fields.items()
            if not k.startswith("_")
        }

    if hasattr(value, "__slots__"):
        slots = getattr(value, "__slots__")
        if isinstance(slots, str):
            slots = [slots]
        return {
            name: _normalize_jsonish(getattr(value, name))
            for name in slots
            if isinstance(name, str)
            and not name.startswith("_")
            and hasattr(value, name)
        }

    return value
