#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ModuleNotFoundError as exc:  # pragma: no cover - user environment guard
    raise SystemExit("PyYAML is required. Install it with 'python3 -m pip install PyYAML'.") from exc

QUICKTYPE_VERSION = "26.0.0"
OPENAPI_TYPESCRIPT_VERSION = "7.13.0"

REPO_ROOT = Path(__file__).resolve().parent.parent
OPENAPI_PATH = REPO_ROOT / "api" / "openapi.yaml"
TMP_SCHEMA_PATH = REPO_ROOT / "sdk" / ".openapi-components.schema.json"

TARGETS = {
    "go": REPO_ROOT / "sdk" / "go" / "openapi" / "models_gen.go",
    "python": REPO_ROOT / "sdk" / "python" / "src" / "trident_indexer" / "openapi_models_gen.py",
    "rust": REPO_ROOT / "sdk" / "rust" / "src" / "openapi_models_gen.rs",
    "typescript": REPO_ROOT / "sdk" / "typescript" / "src" / "api-types.gen.ts",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Generate SDK model files from api/openapi.yaml")
    parser.add_argument(
        "--language",
        choices=["all", *TARGETS.keys()],
        default="all",
        help="Generate models for a single SDK or for all SDKs",
    )
    return parser.parse_args()


def rewrite_refs(value):
    if isinstance(value, dict):
        rewritten = {}
        for key, item in value.items():
            if key == "$ref" and isinstance(item, str) and item.startswith("#/components/schemas/"):
                rewritten[key] = item.replace("#/components/schemas/", "#/$defs/")
            else:
                rewritten[key] = rewrite_refs(item)
        return rewritten
    if isinstance(value, list):
        return [rewrite_refs(item) for item in value]
    return value


def build_schema_wrapper() -> dict:
    document = yaml.safe_load(OPENAPI_PATH.read_text(encoding="utf-8"))
    components = rewrite_refs(document["components"]["schemas"])
    for name, schema in components.items():
        if isinstance(schema, dict) and "title" not in schema:
            schema["title"] = name

    return {
        "$schema": "https://json-schema.org/draft/2020-12/schema",
        "title": "OpenAPIModels",
        "type": "object",
        "properties": {name: {"$ref": f"#/$defs/{name}"} for name in sorted(components)},
        "$defs": components,
    }


def write_wrapper_schema() -> None:
    TMP_SCHEMA_PATH.parent.mkdir(parents=True, exist_ok=True)
    TMP_SCHEMA_PATH.write_text(
        json.dumps(build_schema_wrapper(), indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def run_checked(command: list[str], *, capture_output: bool = False) -> str:
    # encoding is pinned rather than left to the platform default: quicktype
    # emits em-dashes from the spec's descriptions, and on a cp1252 default
    # those round-trip as mojibake, so the generated files differ from what
    # CI produces and the drift check below fails with no real change.
    result = subprocess.run(
        command,
        cwd=REPO_ROOT,
        check=True,
        text=True,
        encoding="utf-8",
        capture_output=capture_output,
    )
    return result.stdout if capture_output else ""


def generate_go() -> None:
    output = run_checked(
        [
            "npx",
            "--yes",
            f"quicktype@{QUICKTYPE_VERSION}",
            "--src-lang",
            "schema",
            "--lang",
            "go",
            "--top-level",
            "OpenAPIModels",
            "--package",
            "openapi",
            str(TMP_SCHEMA_PATH),
        ],
        capture_output=True,
    )
    TARGETS["go"].parent.mkdir(parents=True, exist_ok=True)
    TARGETS["go"].write_text(output, encoding="utf-8", newline="\n")


def generate_python() -> None:
    output = run_checked(
        [
            "npx",
            "--yes",
            f"quicktype@{QUICKTYPE_VERSION}",
            "--src-lang",
            "schema",
            "--lang",
            "py",
            "--top-level",
            "OpenAPIModels",
            "--no-date-times",
            str(TMP_SCHEMA_PATH),
        ],
        capture_output=True,
    )
    TARGETS["python"].write_text(output, encoding="utf-8", newline="\n")


def generate_rust() -> None:
    output = run_checked(
        [
            "npx",
            "--yes",
            f"quicktype@{QUICKTYPE_VERSION}",
            "--src-lang",
            "schema",
            "--lang",
            "rs",
            "--top-level",
            "OpenAPIModels",
            str(TMP_SCHEMA_PATH),
        ],
        capture_output=True,
    )
    TARGETS["rust"].write_text(output, encoding="utf-8", newline="\n")

    # Run rustfmt over the result so this file is byte-identical to what
    # `cargo fmt --all` produces. Without it the two CI jobs contradict each
    # other: quicktype emits `use serde::{Serialize, Deserialize};`, rustfmt
    # reorders that to `{Deserialize, Serialize}`, and whichever job ran last
    # left the other one red — the Rust job on a formatting diff, or this
    # script's own drift check on the reordered import.
    run_checked(["rustfmt", "--edition", "2021", str(TARGETS["rust"])])


def generate_typescript() -> None:
    run_checked(
        [
            "npx",
            "--yes",
            f"openapi-typescript@{OPENAPI_TYPESCRIPT_VERSION}",
            str(OPENAPI_PATH),
            "-o",
            str(TARGETS["typescript"]),
        ]
    )


def main() -> int:
    args = parse_args()
    selected = list(TARGETS) if args.language == "all" else [args.language]

    needs_wrapper = any(language in {"go", "python", "rust"} for language in selected)
    try:
        if needs_wrapper:
            write_wrapper_schema()

        for language in selected:
            {
                "go": generate_go,
                "python": generate_python,
                "rust": generate_rust,
                "typescript": generate_typescript,
            }[language]()
    finally:
        if TMP_SCHEMA_PATH.exists():
            TMP_SCHEMA_PATH.unlink()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
