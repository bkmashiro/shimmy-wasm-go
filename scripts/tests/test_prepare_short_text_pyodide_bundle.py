from __future__ import annotations

import importlib.util
from pathlib import Path


SCRIPT = Path(__file__).resolve().parents[1] / "prepare-short-text-pyodide-bundle.py"
SPEC = importlib.util.spec_from_file_location("prepare_short_text_pyodide_bundle", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def test_prepare_copies_runtime_files_and_populates_nltk_data(tmp_path, monkeypatch) -> None:
    source = tmp_path / "source"
    source.mkdir()
    for name in MODULE.RUNTIME_FILES:
        (source / name).write_text(name)

    downloads = []

    def create_asset(kind: str, package: str, destination: Path) -> None:
        downloads.append((kind, package))
        marker = next(
            marker
            for row_kind, row_package, marker in MODULE.NLTK_ASSETS
            if (row_kind, row_package) == (kind, package)
        )
        target = destination / "nltk_data" / kind / package / marker
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("ready")

    monkeypatch.setattr(MODULE, "download_asset", create_asset)

    out = tmp_path / "bundle"
    MODULE.prepare(source, out)

    for name in MODULE.RUNTIME_FILES:
        assert (out / name).read_text() == name
    assert downloads == [
        ("models", "word2vec_sample"),
        ("corpora", "stopwords"),
        ("tokenizers", "punkt"),
    ]
    for kind, package, marker in MODULE.NLTK_ASSETS:
        assert (out / "nltk_data" / kind / package / marker).read_text() == "ready"
