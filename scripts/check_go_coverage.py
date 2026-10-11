#!/usr/bin/env python3
"""Enforce statement coverage, merging duplicate blocks across Go packages."""
import argparse
from collections import defaultdict
from pathlib import Path
import re
import sys

BLOCK = re.compile(r"^(.+\.go):\d+\.\d+,\d+\.\d+$")


def coverage(text):
    lines = text.splitlines()
    if not lines or lines[0] not in {"mode: set", "mode: count", "mode: atomic"}:
        raise ValueError("missing or invalid Go coverage mode")
    blocks = {}
    for line in lines[1:]:
        parts = line.split()
        if len(parts) != 3 or not BLOCK.fullmatch(parts[0]):
            raise ValueError("invalid coverage block")
        location, statements, count = parts[0], int(parts[1]), int(parts[2])
        if statements < 0 or count < 0:
            raise ValueError("negative coverage value")
        if location in blocks:
            previous, hits = blocks[location]
            if previous != statements:
                raise ValueError("inconsistent duplicate coverage block")
            count = max(hits, count)
        blocks[location] = statements, count
    result = defaultdict(lambda: [0, 0])
    for location, (statements, hits) in blocks.items():
        filename = BLOCK.fullmatch(location).group(1)
        for scope in ("total", filename.rsplit("/", 1)[0]):
            result[scope][0] += statements
            result[scope][1] += statements if hits else 0
    if result["total"][0] == 0:
        raise ValueError("empty coverage profile")
    return dict(result)


def check(stats, minimum=90):
    module = "github.com/metasequoiaime/MSIME-Backend"
    # Keep every executable package in the denominator, including the CLI.
    for package in (module, module + "/cmd/msime-server", module + "/cmd/msime-cloud", module + "/admin-web",
                    module + "/internal/account", module + "/internal/server",
                    module + "/internal/engine", module + "/internal/skins"):
        if package not in stats or stats[package][0] <= 0:
            raise ValueError("coverage profile is missing package: " + package)
    for scope in ("total", module + "/internal/account", module + "/internal/server"):
        total, covered = stats[scope]
        if covered * 100 < total * minimum:
            raise ValueError(f"{scope}: {covered / total:.2%} is below {minimum}%")


# 不带原生引擎、只跑 internal/account 与 internal/server 时的下限（「用户体系 PostgreSQL 集成测试」）。这两个包有一部分只在真实引擎下执行，2026-10 实测分别是 87.2% 和 90.6%，下限各留约两个半百分点。
DATABASE_MINIMUMS = {"/internal/account": 85, "/internal/server": 88}


def check_database(stats):
    module = "github.com/metasequoiaime/MSIME-Backend"
    for suffix, minimum in DATABASE_MINIMUMS.items():
        scope = module + suffix
        if scope not in stats or stats[scope][0] <= 0:
            raise ValueError("coverage profile is missing package: " + scope)
        total, covered = stats[scope]
        if covered * 100 < total * minimum:
            raise ValueError(f"{scope}: {covered / total:.2%} is below {minimum}%")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("profile", type=Path)
    parser.add_argument("--database", action="store_true",
                        help="只检查 internal/account 与 internal/server，按不带原生引擎时的下限")
    args = parser.parse_args()
    try:
        stats = coverage(args.profile.read_text())
        check_database(stats) if args.database else check(stats)
    except (OSError, ValueError) as error:
        print(f"Coverage gate failed: {error}", file=sys.stderr)
        return 1
    if args.database:
        limits = ", ".join(f"{suffix.rsplit('/', 1)[1]} >= {minimum}%" for suffix, minimum in DATABASE_MINIMUMS.items())
        print(f"Go statement coverage passed without the native engine: {limits}")
        return 0
    total, covered = stats["total"]
    print(f"Go statement coverage passed: {covered}/{total} ({covered / total:.2%}); account/server >= 90%")
    return 0


if __name__ == "__main__":
    sys.exit(main())
