"""Trusted bootstrap for the Shimmy Python/WASI guest.

This source is embedded at build time and executed once during Guest init.
Evaluator source is supplied separately at the trusted preparation boundary.
"""

from __future__ import annotations

import json as _json


_prepared_eval = None
_prepared_preview = None


def _shimmy_prepare(source: str) -> None:
    global _prepared_eval, _prepared_preview
    if not isinstance(source, str):
        raise TypeError("evaluator source must be text")
    namespace = {"__builtins__": __builtins__, "__name__": "__shimmy_evaluator__"}
    exec(compile(source, "<shimmy-evaluator>", "exec"), namespace, namespace)
    evaluation = namespace.get("evaluation_function")
    preview = namespace.get("preview_function")
    if not callable(evaluation):
        raise ValueError("evaluator must define evaluation_function")
    if preview is not None and not callable(preview):
        raise ValueError("preview_function must be callable when defined")
    _prepared_eval = evaluation
    _prepared_preview = preview


def _json_default(value):
    item = getattr(value, "item", None)
    if callable(item):
        return item()
    tolist = getattr(value, "tolist", None)
    if callable(tolist):
        return tolist()
    raise TypeError(f"value of type {type(value).__name__} is not JSON serializable")


def _error(exc: BaseException) -> str:
    payload = {
        "status": "error",
        "error": {
            "type": type(exc).__name__,
            "message": str(exc)[:4096],
        },
    }
    return _json.dumps(payload, ensure_ascii=False, separators=(",", ":"))


def _shimmy_handle_request(request_json: str) -> str:
    try:
        if _prepared_eval is None:
            raise RuntimeError("evaluator has not been prepared")
        request = _json.loads(request_json)
        if not isinstance(request, dict):
            raise ValueError("request must be an object")
        if set(request) != {"method", "params"}:
            raise ValueError("request must contain exactly method and params")
        method = request["method"]
        params = request["params"]
        if method not in {"eval", "preview"}:
            raise ValueError("method must be eval or preview")
        if not isinstance(params, dict):
            raise ValueError("params must be an object")

        if method == "preview" and _prepared_preview is not None:
            result = _prepared_preview(
                params.get("response"),
                params.get("params", {}),
            )
        else:
            result = _prepared_eval(
                params.get("response"),
                params.get("answer"),
                params.get("params", {}),
            )
        return _json.dumps(
            {"status": "ok", "result": result},
            default=_json_default,
            ensure_ascii=False,
            separators=(",", ":"),
        )
    except Exception as exc:
        return _error(exc)
