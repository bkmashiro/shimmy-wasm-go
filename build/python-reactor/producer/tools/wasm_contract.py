#!/usr/bin/env python3
"""Minimal strict WebAssembly import/export inspector for producer gates."""

from __future__ import annotations

from dataclasses import dataclass


KIND_NAMES = {0: "function", 1: "table", 2: "memory", 3: "global", 4: "tag"}


@dataclass
class Reader:
    data: bytes
    offset: int = 0

    def take(self, count: int) -> bytes:
        if count < 0 or self.offset + count > len(self.data):
            raise ValueError("truncated WebAssembly payload")
        result = self.data[self.offset : self.offset + count]
        self.offset += count
        return result

    def byte(self) -> int:
        return self.take(1)[0]

    def uleb(self, max_bytes: int = 5) -> int:
        value = 0
        shift = 0
        for _ in range(max_bytes):
            byte = self.byte()
            value |= (byte & 0x7F) << shift
            if byte & 0x80 == 0:
                return value
            shift += 7
        raise ValueError("invalid unsigned LEB128")

    def name(self) -> str:
        raw = self.take(self.uleb())
        try:
            return raw.decode("utf-8", "strict")
        except UnicodeDecodeError as exc:
            raise ValueError("invalid UTF-8 WebAssembly name") from exc


def _skip_limits(reader: Reader) -> None:
    flags = reader.uleb()
    reader.uleb()
    if flags & 0x01:
        reader.uleb()


def _skip_import_descriptor(reader: Reader, kind: int) -> None:
    if kind == 0:
        reader.uleb()
    elif kind == 1:
        reader.byte()
        _skip_limits(reader)
    elif kind == 2:
        _skip_limits(reader)
    elif kind == 3:
        reader.take(2)
    elif kind == 4:
        reader.byte()
        reader.uleb()
    else:
        raise ValueError(f"unknown WebAssembly import kind {kind}")


def inspect_wasm(data: bytes) -> dict[str, list[dict[str, str]]]:
    reader = Reader(data)
    if reader.take(4) != b"\x00asm" or reader.take(4) != b"\x01\x00\x00\x00":
        raise ValueError("invalid WebAssembly magic or version")

    imports: list[dict[str, str]] = []
    exports: list[dict[str, str]] = []
    previous_noncustom = 0
    while reader.offset < len(data):
        section_id = reader.byte()
        section_size = reader.uleb()
        section = Reader(reader.take(section_size))
        if section_id != 0:
            if section_id < previous_noncustom:
                raise ValueError("invalid WebAssembly section order")
            previous_noncustom = section_id
        if section_id == 2:
            for _ in range(section.uleb()):
                module = section.name()
                name = section.name()
                kind = section.byte()
                _skip_import_descriptor(section, kind)
                imports.append({"module": module, "name": name, "kind": KIND_NAMES[kind]})
        elif section_id == 7:
            for _ in range(section.uleb()):
                name = section.name()
                kind = section.byte()
                section.uleb()
                if kind not in KIND_NAMES:
                    raise ValueError(f"unknown WebAssembly export kind {kind}")
                exports.append({"name": name, "kind": KIND_NAMES[kind]})
        if section.offset != len(section.data) and section_id in {2, 7}:
            raise ValueError("trailing bytes in parsed WebAssembly section")
    return {"imports": imports, "exports": exports}


def verify_shape(
    shape: dict[str, list[dict[str, str]]],
    *,
    required_exports: list[str],
    allowed_import_modules: list[str],
) -> None:
    imported_modules = {item["module"] for item in shape["imports"]}
    forbidden = sorted(imported_modules - set(allowed_import_modules))
    if forbidden:
        raise ValueError(f"forbidden import module(s): {', '.join(forbidden)}")
    exported = {item["name"] for item in shape["exports"]}
    missing = sorted(set(required_exports) - exported)
    if missing:
        raise ValueError(f"missing exports: {', '.join(missing)}")
