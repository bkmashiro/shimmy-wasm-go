from __future__ import annotations

import json
from pathlib import Path


ROOT = Path(__file__).resolve().parent
MATRIX = ROOT / "capability-matrix.json"


def test_capability_matrix_schema_and_fixture_paths() -> None:
    rows = json.loads(MATRIX.read_text())
    assert rows, "matrix must not be empty"

    names = {row["fixture"] for row in rows}
    assert len(names) == len(rows), "fixture names must be unique"
    assert {
        "boilerplate-python",
        "compare-boolean",
        "array-equal",
        "is-similar",
        "symbolic-equal",
        "short-text-answer",
    } <= names

    for row in rows:
        assert row["status"] in {
            "agent-python-qualified",
            "agent-python-candidate",
            "not-qualified",
            "pyodide-only",
            "out-of-scope",
        }
        assert isinstance(row["agent_python_bundle"], bool)
        assert isinstance(row["pyodide_package"], bool)
        assert isinstance(row["requires_pure_python_deps"], list)
        assert isinstance(row["requires_agent_artifact_deps"], list)
        assert isinstance(row["unqualified_reasons"], list)
        assert isinstance(row["verified_probes"], list)

        fixture_path = ROOT / row["fixture"]
        if row["agent_python_bundle"]:
            assert fixture_path.exists(), f"Agent Python fixture missing: {fixture_path}"
        if row["status"] == "agent-python-qualified":
            assert row["agent_python_bundle"]
            assert row["verified_probes"], f"qualified fixture lacks probes: {row['fixture']}"
            assert not row["unqualified_reasons"]
        else:
            assert row["unqualified_reasons"], f"unqualified row lacks reasons: {row['fixture']}"


def test_capability_matrix_matches_agent_python_qualification() -> None:
    rows = {row["fixture"]: row for row in json.loads(MATRIX.read_text())}

    assert rows["boilerplate-python"]["status"] == "agent-python-qualified"
    assert rows["array-equal"]["status"] == "agent-python-qualified"
    assert rows["is-similar"]["status"] == "agent-python-qualified"
    assert rows["compare-boolean"]["status"] == "not-qualified"
    assert rows["symbolic-equal"]["status"] == "not-qualified"
    assert rows["short-text-answer"]["status"] == "pyodide-only"
