"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

const { buildPackageBootstrap } = require("./package-bootstrap");

test("package bootstrap uses evaluator root for relative assets", () => {
  const source = buildPackageBootstrap({
    evaluatorRoot: "/__evaluator_root__",
    adapterRoot: "/__lf_adapter_root__",
    adapterPath: "/__lf_compat_adapter__.py",
  });

  assert.match(source, /os\.chdir\("\/__evaluator_root__"\)/);
});

test("package bootstrap exposes bundled NLTK data when present", () => {
  const source = buildPackageBootstrap({
    evaluatorRoot: "/__evaluator_root__",
    adapterRoot: "/__lf_adapter_root__",
    adapterPath: "/__lf_compat_adapter__.py",
  });

  assert.match(source, /os\.path\.isdir\(_nltk_data\)/);
  assert.match(source, /os\.environ\["NLTK_DATA"\] = _nltk_data/);
});
