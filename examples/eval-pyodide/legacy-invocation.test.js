"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

const { buildLegacyInvocation } = require("./legacy-invocation");

test("legacy invocation accepts evaluation_function scripts", () => {
  const source = buildLegacyInvocation();
  assert.match(source, /_ns\.get\("evaluation_function"\)/);
});

test("legacy invocation accepts reactor-style dispatch scripts", () => {
  const source = buildLegacyInvocation();
  assert.match(source, /_ns\.get\("dispatch"\)/);
  assert.match(source, /_dispatch\(__method__, _payload\)/);
});
