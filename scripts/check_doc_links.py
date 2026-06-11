#!/usr/bin/env python3
"""Hermetic internal-link checker for Nova's Markdown docs.

Validates that every *local* Markdown link target (relative paths and
repo-root-absolute `/docs/...` style) resolves to a file that exists. External
links (http/https/mailto), pure `#anchor` links, and protocol-relative links
are skipped — this checker is about local file integrity, not reachability, so
it stays fast, offline, and non-flaky in CI.

Usage:  python3 scripts/check_doc_links.py [root_dir ...]   (default: docs)
Exit:   0 if all local links resolve, 1 otherwise (prints each broken link).
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

# Inline `](target)` links and reference-style `[id]: target` definitions.
INLINE = re.compile(r"\]\(([^)]+)\)")
REFDEF = re.compile(r"^\s*\[[^\]]+\]:\s+(\S+)", re.MULTILINE)
# Fenced code blocks (``` … ```) and inline code spans (` … `). Stripping these
# before link extraction avoids false positives from code like `ptr[T any](v T)`.
FENCED = re.compile(r"```.*?```", re.DOTALL)
INLINE_CODE = re.compile(r"`[^`]*`")

SKIP_PREFIXES = ("http://", "https://", "mailto:", "#", "//", "tel:")
REPO_ROOT = Path(__file__).resolve().parent.parent


def strip_code(text: str) -> str:
    text = FENCED.sub("", text)
    return INLINE_CODE.sub("", text)


def link_targets(text: str):
    for m in INLINE.finditer(text):
        yield m.group(1).strip()
    for m in REFDEF.finditer(text):
        yield m.group(1).strip()


def is_local_path(target: str) -> bool:
    if not target or target.startswith(SKIP_PREFIXES):
        return False
    if "://" in target:
        return False
    return True


def main(argv: list[str]) -> int:
    roots = [Path(a) for a in argv[1:]] or [Path("docs")]
    md_files: list[Path] = []
    for root in roots:
        md_files.extend(sorted(root.rglob("*.md")))

    broken: list[str] = []
    for md in md_files:
        text = strip_code(md.read_text(encoding="utf-8", errors="replace"))
        for target in link_targets(text):
            if not is_local_path(target):
                continue
            # Drop link title syntax: ](path "title")  ->  path
            path_part = target.split()[0]
            # Strip any #anchor fragment.
            path_part = path_part.split("#", 1)[0]
            if not path_part:
                continue
            if path_part.startswith("/"):
                candidate = REPO_ROOT / path_part.lstrip("/")
            else:
                candidate = md.parent / path_part
            if not candidate.exists():
                broken.append(f"{md}: -> {target}")

    if broken:
        print("Broken local Markdown links:")
        for b in broken:
            print(f"  {b}")
        print(f"\n{len(broken)} broken link(s) across {len(md_files)} file(s).")
        return 1

    print(f"OK: all local Markdown links resolve across {len(md_files)} file(s).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
