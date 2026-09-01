#!/usr/bin/env python3
"""Prepare the data-bearing shortTextAnswer bundle for Pyodide."""

from __future__ import annotations

import argparse
from pathlib import Path
import shutil
import tempfile
from urllib.request import urlopen
from zipfile import ZipFile


NLTK_ASSETS = (
    ("models", "word2vec_sample", "pruned.word2vec.txt"),
    ("corpora", "stopwords", "english"),
    ("tokenizers", "punkt", "english.pickle"),
)
BASE_URL = "https://raw.githubusercontent.com/nltk/nltk_data/gh-pages/packages"
RUNTIME_FILES = ("evaluation.py", "brown_length", "word_freqs")


def download_asset(kind: str, package: str, destination: Path) -> None:
    target_dir = destination / "nltk_data" / kind
    target_dir.mkdir(parents=True, exist_ok=True)
    url = f"{BASE_URL}/{kind}/{package}.zip"
    with tempfile.NamedTemporaryFile(suffix=".zip") as archive:
        with urlopen(url, timeout=120) as response:
            shutil.copyfileobj(response, archive)
        archive.flush()
        with ZipFile(archive.name) as zipped:
            zipped.extractall(target_dir)


def prepare(source: Path, destination: Path) -> None:
    source = source.resolve()
    destination = destination.resolve()
    destination.mkdir(parents=True, exist_ok=True)

    for name in RUNTIME_FILES:
        source_file = source / name
        if not source_file.is_file():
            raise FileNotFoundError(f"missing shortTextAnswer runtime file: {source_file}")
        shutil.copy2(source_file, destination / name)

    for kind, package, marker in NLTK_ASSETS:
        expected = destination / "nltk_data" / kind / package / marker
        if not expected.is_file():
            download_asset(kind, package, destination)
        if not expected.is_file():
            raise RuntimeError(f"NLTK package did not provide expected file: {expected}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    prepare(args.source, args.out)
    print(args.out.resolve())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
