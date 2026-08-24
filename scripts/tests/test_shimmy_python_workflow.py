from __future__ import annotations

import pathlib
import re
import stat
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "build.yml"


class ShimmyPythonWorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.text = WORKFLOW.read_text()

    def test_manual_mode_is_explicit_and_standard_is_default(self) -> None:
        prefix = self.text.split("jobs:", 1)[0]
        self.assertIn("workflow_dispatch:", prefix)
        self.assertRegex(prefix, r"mode:\s*\n\s+description:")
        self.assertIn("default: standard", prefix)
        self.assertIn("- shimmy-python", prefix)

    def test_artifact_build_is_manual_mode_only(self) -> None:
        match = re.search(
            r"^  build-shimmy-python:\n(?P<body>.*?)(?=^  [a-zA-Z0-9_-]+:|\Z)",
            self.text,
            re.MULTILINE | re.DOTALL,
        )
        if match is None:
            self.fail("missing workflow job build-shimmy-python")
        body = match.group("body")
        self.assertIn("github.event_name == 'workflow_dispatch'", body)
        self.assertIn("inputs.mode == 'shimmy-python'", body)

    def test_source_bound_artifact_is_built_and_uploaded(self) -> None:
        launcher = ROOT / "build/python-reactor/producer/build/build-base.sh"
        self.assertTrue(launcher.stat().st_mode & stat.S_IXUSR)
        self.assertIn("build/python-reactor/producer/build/build-base.sh", self.text)
        self.assertIn("build/python-reactor/producer/build/build-numpy-core.sh", self.text)
        self.assertIn("build/python-reactor/producer/build/build-sympy.sh", self.text)
        self.assertIn("dist/shimmy-python/base", self.text)
        self.assertIn("dist/shimmy-python/numpy-core", self.text)
        self.assertIn("dist/shimmy-python/sympy", self.text)
        self.assertIn("actions/upload-artifact@v4", self.text)
        self.assertIn("retention-days: 7", self.text)
        self.assertIn("sha256sum -c SHA256SUMS", self.text)

    def test_host_consumer_is_deferred_to_the_host_pr(self) -> None:
        self.assertNotIn("test-shimmy-python", self.text)
        self.assertNotIn("SHIMMY_PYTHON_RUNTIME_ARTIFACT", self.text)
        self.assertNotIn("TestShimmyPythonArtifactE2E", self.text)

    def test_no_cross_project_or_release_path(self) -> None:
        lowered = self.text.lower()
        self.assertNotIn("agent-python-runtime", lowered)
        self.assertNotIn("webassembly-language-runtimes", lowered)
        self.assertNotIn("git lfs pull", lowered)
        if "  build-shimmy-python:" not in self.text:
            self.fail("missing workflow job build-shimmy-python")
        artifact_jobs = self.text.split("  build-shimmy-python:", 1)[1]
        self.assertNotIn("release", artifact_jobs.lower())
        self.assertNotRegex(artifact_jobs, r"checkout@[^\n]+\n(?:.*\n){0,8}\s+repository:")


if __name__ == "__main__":
    unittest.main()
