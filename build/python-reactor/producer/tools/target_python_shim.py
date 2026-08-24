#!/usr/bin/env python3
"""Native codegen Python that reports target CPython metadata to Meson."""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import sys


def main() -> int:
    if len(sys.argv) == 2 and pathlib.Path(sys.argv[1]).name == "python_info.py":
        completed = subprocess.run(
            [sys.executable, sys.argv[1]], text=True, capture_output=True, check=True
        )
        info = json.loads(completed.stdout)
        include = os.environ["SHIMMY_TARGET_PYTHON_INCLUDE"]
        platinclude = os.environ["SHIMMY_TARGET_PYTHON_PLATINCLUDE"]
        info.update(
            {
                "version": "3.14",
                "platform": "wasm32-wasip1",
                "suffix": ".so",
                "limited_api_suffix": ".abi3.so",
                "is_pypy": False,
                "is_freethreaded": False,
                "is_venv": False,
                "link_libpython": False,
            }
        )
        info["variables"].update(
            {
                "INCLUDEPY": include,
                "LIBPC": "",
                "prefix": "/usr/local",
                "base_prefix": "/usr/local",
                "py_version_short": "3.14",
                "LDVERSION": "3.14",
            }
        )
        info["paths"].update(
            {
                "include": include,
                "platinclude": platinclude,
                "purelib": "/usr/local/lib/python3.14/site-packages",
                "platlib": "/usr/local/lib/python3.14/site-packages",
            }
        )
        info["sysconfig_paths"].update(info["paths"])
        print(json.dumps(info, sort_keys=True))
        return 0
    os.execv(sys.executable, [sys.executable, *sys.argv[1:]])
    return 127


if __name__ == "__main__":
    raise SystemExit(main())
