"use strict";

/**
 * Pyodide/Node.js runner for shimmy eval functions.
 *
 * Protocol: go-ethereum JSON-RPC 2.0 over stdio, framed with LSP-style headers.
 *
 * Each message (both directions) is framed as:
 *   Content-Length: <N>\r\n
 *   <N bytes of JSON>
 *
 * Request JSON (from shimmy):
 *   {"jsonrpc":"2.0","id":<id>,"method":"<method>","params":[{...}]}
 *
 * Response JSON (to shimmy):
 *   {"jsonrpc":"2.0","id":<id>,"result":{...}}
 *   {"jsonrpc":"2.0","id":<id>,"error":{"code":<int>,"message":"<str>"}}
 *
 * Supported modes:
 *   - Legacy script mode:
 *       node runner.js /path/to/eval.py
 *       or FUNCTION_PYODIDE_SCRIPT=/path/to/eval.py node runner.js
 *     The script must define evaluation_function(response, answer, params).
 *
 *   - Lambda Feedback package mode:
 *       FUNCTION_PYODIDE_ROOT=/path/to/evaluator/root \
 *       FUNCTION_PYODIDE_EVAL_ENTRYPOINT=evaluation_function.evaluation:evaluation_function \
 *       FUNCTION_PYODIDE_PREVIEW_ENTRYPOINT=evaluation_function.preview:preview_function \
 *       FUNCTION_PYODIDE_ADAPTER=/path/to/lf_compat_adapter.py
 *     with no script arg.
 *     The evaluator package is mirrored into the Pyodide FS and loaded via
 *     lf_compat_adapter.call_function + normalize_result.
 */

const { loadPyodide } = require("pyodide");
const fs = require("fs");
const path = require("path");
const {
  DEFAULT_MAX_FRAME_BYTES,
  FramedReader,
  encodeFrame,
} = require("./framed-stdio");
const { parsePackages } = require("./package-config");
const { buildPackageBootstrap } = require("./package-bootstrap");
const { buildLegacyInvocation } = require("./legacy-invocation");

const VFS_ROOT = "/__evaluator_root__";
const ADAPTER_VFS_ROOT = "/__lf_adapter_root__";
const ADAPTER_VFS_PATH = "/__lf_compat_adapter__.py";

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const legacyScriptPath = process.argv[2] || process.env.FUNCTION_PYODIDE_SCRIPT;
const packageRootPath = process.env.FUNCTION_PYODIDE_ROOT;
const evalEntrypoint = process.env.FUNCTION_PYODIDE_EVAL_ENTRYPOINT;
const previewEntrypoint = process.env.FUNCTION_PYODIDE_PREVIEW_ENTRYPOINT;
const adapterPath = process.env.FUNCTION_PYODIDE_ADAPTER;

const packageMode = Boolean(packageRootPath && evalEntrypoint);
const legacyMode = Boolean(legacyScriptPath) && !packageMode;

function errorAndExit(message, code = 1) {
  process.stderr.write(`${message}\n`);
  process.exit(code);
}

if (!legacyMode && !packageMode) {
  errorAndExit(
    "Usage: node runner.js <eval.py> " +
      "or set FUNCTION_PYODIDE_SCRIPT, or set FUNCTION_PYODIDE_ROOT + FUNCTION_PYODIDE_EVAL_ENTRYPOINT"
  );
}

let evalCode;
let resolvedScriptPath;
let resolvedRootPath;
let resolvedAdapterPath;

if (packageMode) {
  if (typeof evalEntrypoint !== "string" || evalEntrypoint.indexOf(":") === -1) {
    errorAndExit(
      "FUNCTION_PYODIDE_EVAL_ENTRYPOINT must be in module:function format"
    );
  }

  resolvedRootPath = path.resolve(packageRootPath);
  if (!fs.existsSync(resolvedRootPath)) {
    errorAndExit(`Package root not found: ${resolvedRootPath}`);
  }

  if (!adapterPath) {
    errorAndExit(
      "FUNCTION_PYODIDE_ADAPTER is required for package mode (e.g. examples/lambda-feedback-adapter/lf_compat_adapter.py)"
    );
  }

  resolvedAdapterPath = path.resolve(adapterPath);
  if (!fs.existsSync(resolvedAdapterPath)) {
    errorAndExit(`Lambda Feedback adapter not found: ${resolvedAdapterPath}`);
  }
} else {
  resolvedScriptPath = path.resolve(legacyScriptPath);
  if (!fs.existsSync(resolvedScriptPath)) {
    errorAndExit(`Script not found: ${resolvedScriptPath}`);
  }
  evalCode = fs.readFileSync(resolvedScriptPath, "utf8");
}

const defaultPackages = legacyMode ? ["scipy"] : [];
const packagesValue = Object.prototype.hasOwnProperty.call(process.env, "FUNCTION_PYODIDE_PACKAGES")
  ? process.env.FUNCTION_PYODIDE_PACKAGES
  : undefined;
const pyodidePackages = parsePackages(packagesValue, defaultPackages);

// ---------------------------------------------------------------------------
// LSP-framed stdio transport
// ---------------------------------------------------------------------------

/**
 * Write a JSON-RPC response to stdout, framed with Content-Length.
 */
function writeMessage(obj) {
  process.stdout.write(
    encodeFrame(JSON.stringify(obj), DEFAULT_MAX_FRAME_BYTES)
  );
}

/**
 * Build a JSON-RPC success response.
 */
function makeResult(id, result) {
  return { jsonrpc: "2.0", id, result };
}

/**
 * Build a JSON-RPC error response.
 */
function makeError(id, code, message, data) {
  const error = { code, message };
  if (data !== undefined) error.data = data;
  return { jsonrpc: "2.0", id, error };
}

// ---------------------------------------------------------------------------
// Framed-message reader
//
// The go-ethereum rpc library writes frames as:
//   Content-Length: N\r\n\r\n<N bytes>
// There may be stray output before the first Content-Length line (e.g. model
// loading logs), which we skip.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Python bootstrap helpers
// ---------------------------------------------------------------------------

/**
 * Copy a host directory tree into Pyodide's virtual FS.
 */
function mirrorDirToPyodide(pyodide, sourcePath, targetPath) {
  const skip = new Set([".git", "__pycache__", ".venv", "node_modules"]);

  const walk = (src, dst) => {
    const dirEntries = fs.readdirSync(src, { withFileTypes: true });
    for (const dirent of dirEntries) {
      if (skip.has(dirent.name)) continue;

      const srcChild = path.join(src, dirent.name);
      const dstChild = path.join(dst, dirent.name);

      if (dirent.isDirectory()) {
        pyodide.FS.mkdirTree(dstChild);
        walk(srcChild, dstChild);
        continue;
      }

      if (dirent.isFile()) {
        const data = fs.readFileSync(srcChild);
        pyodide.FS.writeFile(dstChild, data);
        continue;
      }
    }
  };

  if (!fs.existsSync(sourcePath)) {
    throw new Error(`Source path does not exist: ${sourcePath}`);
  }

  if (!fs.statSync(sourcePath).isDirectory()) {
    throw new Error(`Source path is not a directory: ${sourcePath}`);
  }

  pyodide.FS.mkdirTree(targetPath);
  walk(sourcePath, targetPath);
}

/**
 * Normalize a Python result returned as a proxy.
 */
function asJs(resultProxy) {
  if (!resultProxy) return resultProxy;

  if (typeof resultProxy.toJs === "function") {
    const value = resultProxy.toJs({ dict_converter: Object.fromEntries });
    resultProxy.destroy();
    return value;
  }

  return resultProxy;
}

/**
 * Resolve request payload from JSON-RPC params.
 */
function requestPayload(request) {
  const { params } = request;
  const data = Array.isArray(params) ? params[0] : params;

  return {
    response: (data && data.response !== undefined ? data.response : null),
    answer: (data && data.answer !== undefined ? data.answer : null),
    params: (data && data.params !== undefined ? data.params : {}),
  };
}

/**
 * Configure package mode inside Pyodide: mount evaluator package and adapter.
 */
async function setupPackageMode(pyodide) {
  process.stderr.write(`Loading evaluator package from ${resolvedRootPath}\n`);
  mirrorDirToPyodide(pyodide, resolvedRootPath, VFS_ROOT);

  // Mirror the adapter directory too, not just lf_compat_adapter.py, because
  // real fixtures import the minimal lf_toolkit shim that lives next to the
  // adapter module.
  mirrorDirToPyodide(pyodide, path.dirname(resolvedAdapterPath), ADAPTER_VFS_ROOT);

  const adapterCode = fs.readFileSync(resolvedAdapterPath, "utf8");
  pyodide.FS.writeFile(ADAPTER_VFS_PATH, adapterCode);

  pyodide.globals.set("__eval_entrypoint__", evalEntrypoint);
  pyodide.globals.set("__preview_entrypoint__", previewEntrypoint || "");

  pyodide.runPython(
    buildPackageBootstrap({
      evaluatorRoot: VFS_ROOT,
      adapterRoot: ADAPTER_VFS_ROOT,
      adapterPath: ADAPTER_VFS_PATH,
    })
  );
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
  process.stderr.write("Loading Pyodide...\n");

  const pyodide = await loadPyodide();

  if (pyodidePackages.length > 0) {
    const packageList = pyodidePackages.join(", ");
    process.stderr.write(`Loading Pyodide packages: ${packageList}...\n`);
    await pyodide.loadPackage(pyodidePackages, {
      messageCallback: (msg) => process.stderr.write(msg + "\n"),
    });
  }

  if (legacyMode) {
    process.stderr.write(`Loading eval script: ${resolvedScriptPath}\n`);

    // Validate the script by running it once in a throw-away namespace.
    try {
      pyodide.runPython("exec(__eval_source__, {})", {
        globals: pyodide.toPy({ __eval_source__: evalCode }),
      });
    } catch (err) {
      process.stderr.write(`Error loading eval script: ${err}\n`);
      process.exit(1);
    }

    process.stderr.write("Ready.\n");
  } else {
    await setupPackageMode(pyodide);
    process.stderr.write("Ready.\n");
  }

  process.stdin.resume();
  const reader = new FramedReader(process.stdin, {
    maxFrameBytes: DEFAULT_MAX_FRAME_BYTES,
  });

  while (true) {
    let msgBuf;
    try {
      msgBuf = await reader.read();
    } catch (err) {
      break;
    }

    let request;
    try {
      request = JSON.parse(msgBuf.toString("utf8"));
    } catch (err) {
      writeMessage(makeError(null, -32700, "Parse error", err.message));
      continue;
    }

    const id = request.id;
    const method = request.method || "";

    if (method === "healthcheck") {
      writeMessage(makeResult(id, { status: "ok" }));
      continue;
    }

    const payload = requestPayload(request);

    try {
      const resolvedMethod = method === "preview" ? "preview" : "eval";
      const result = await handleRequest(pyodide, resolvedMethod, payload);
      writeMessage(makeResult(id, result));
    } catch (err) {
      writeMessage(makeError(id, -32603, String(err)));
      continue;
    }
  }
}

/**
 * Dispatch one JSON-RPC request to the Python eval function.
 */
async function handleRequest(pyodide, method, payload) {
  if (legacyMode) {
    // Legacy single-file mode accepts either the reactor dispatch ABI or
    // evaluation_function() while keeping a fresh namespace per request.
    const ns = pyodide.toPy({
      __eval_source__: evalCode,
      __method__: method,
      _response: payload.response,
      _answer: payload.answer,
      _params: payload.params,
    });

    try {
      const resultProxy = pyodide.runPython(buildLegacyInvocation(), { globals: ns });

      return asJs(resultProxy);
    } finally {
      ns.destroy();
    }
  }

  const paramsProxy = pyodide.toPy(payload.params ?? {});
  try {
    pyodide.globals.set("__method__", method);
    pyodide.globals.set("__response__", payload.response);
    pyodide.globals.set("__answer__", payload.answer);
    pyodide.globals.set("__params__", paramsProxy);

    const resultProxy = pyodide.runPython(
      `__lf_invoke(__method__, __response__, __answer__, __params__)`
    );
    return asJs(resultProxy);
  } finally {
    paramsProxy.destroy();
  }
}

main().catch((err) => {
  process.stderr.write(`Fatal: ${err}\n`);
  process.exit(1);
});
