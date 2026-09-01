"use strict";

function buildLegacyInvocation() {
  return `
# Fresh namespace - state isolation without a memory snapshot.
_ns = {}
exec(__eval_source__, _ns)

_dispatch = _ns.get("dispatch")
_fn = _ns.get("evaluation_function")
_payload = {"response": _response, "answer": _answer, "params": _params}

if _dispatch is not None:
    _result = _dispatch(__method__, _payload)
elif _fn is not None:
    _result = _fn(_response, _answer, _params)
else:
    raise RuntimeError("eval script defines neither dispatch() nor evaluation_function()")
_result
`;
}

module.exports = { buildLegacyInvocation };
