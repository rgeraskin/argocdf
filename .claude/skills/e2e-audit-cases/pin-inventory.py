#!/usr/bin/env python3
"""Inventory every pinned e2e report: app sections, resource keys with change type,
lint warnings, summary counts, exit code.

This is the direction-B tool of the case audit. The pins record what argocdf
produced; printing them as structure (rather than as diff text) makes it possible to
read a report AGAINST its CASES.md row and spot content that no row explains.
Extras have no other detector: nothing in the gate asserts exhaustiveness, and a
brand-new case has no baseline diff in which an extra section would stand out.

Usage:  python3 pin-inventory.py [EXPECTED_DIR]
        (default: e2e/expected from the argocdf repo root, else ./expected)
"""

import os
import re
import sys

HUNK = re.compile(r"@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@")


def change_type(hunk: str) -> str:
    """+ added, - removed, ~ modified, inferred from the hunk's line counts.

    A resource absent on one side renders as a single-line range on that side:
    "@@ -1 +1,18 @@" is an addition, "@@ -1,18 +1 @@" a removal.
    """
    m = HUNK.match(hunk)
    if not m:
        return "~"
    base_count, target_count = m.group(2), m.group(4)
    if base_count is None and target_count is not None:
        return "+"
    if base_count is not None and target_count is None:
        return "-"
    return "~"


def inventory(case_dir: str) -> None:
    ud = os.path.join(case_dir, "reports", "unified.diff")
    meta = os.path.join(case_dir, "reports", "meta.yaml")
    name = os.path.basename(case_dir.rstrip("/"))
    if not os.path.exists(ud):
        print("### %s  [NO PINNED REPORT]" % name)
        return

    rc = "?"
    if os.path.exists(meta):
        for line in open(meta):
            if line.startswith("exit:"):
                rc = line.split(":", 1)[1].strip()

    lines = open(ud).read().split("\n")
    apps = []  # [name, [resources], [warnings], is_error_section]
    affected = changed = resources = errors = ""

    for i, line in enumerate(lines):
        if line.startswith("# Application: "):
            apps.append([line[len("# Application: "):], [], [], False])
        elif line.startswith("#   • ") and apps:
            apps[-1][2].append(line[6:])
        elif line.startswith("# Error: ") and apps:
            apps[-1][3] = True
        elif line.startswith("--- base/") and apps:
            hunk = next((l for l in lines[i + 1:i + 4] if l.startswith("@@")), "")
            apps[-1][1].append(change_type(hunk) + " " + line[len("--- base/"):])
        elif line.startswith("# Applications affected:"):
            affected = line.split(":", 1)[1].strip()
        elif line.startswith("# Applications changed:"):
            changed = line.split(":", 1)[1].strip()
        elif line.startswith("# Resources:"):
            resources = line.split(":", 1)[1].strip()
        elif line.startswith("# Errors:"):
            errors = line.split(":", 1)[1].strip()

    print("### %s  [affected=%s changed=%s | %s | exit=%s%s]" % (
        name, affected, changed, resources or "no resource line", rc,
        " errors=" + errors if errors else ""))
    for app_name, keys, warns, is_error in apps:
        print("    app %s%s%s" % (
            app_name,
            " ERROR-SECTION" if is_error else "",
            "" if keys or warns else "  (no changes)"))
        for w in warns:
            print("        warn %s" % w)
        for k in keys:
            print("        %s" % k)


def main() -> int:
    root = sys.argv[1] if len(sys.argv) > 1 else (
        "e2e/expected" if os.path.isdir("e2e/expected") else "expected")
    if not os.path.isdir(root):
        print("no expected/ directory found (looked at %r)" % root, file=sys.stderr)
        return 1
    for name in sorted(os.listdir(root)):
        case_dir = os.path.join(root, name)
        if os.path.isdir(case_dir):
            inventory(case_dir)
    return 0


if __name__ == "__main__":
    sys.exit(main())
