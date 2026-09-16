#!/usr/bin/env python3
"""Verify the Chinese strings added by the credits<->tokens feature are valid
UTF-8 and render as intended.

The repo carries pre-existing mojibake in older comments (e.g. "鈮?" where "≈"
was meant, from a commit made with a non-UTF-8 console). This guards against
adding more of it: every string below is read back from the file and must match
exactly, which fails for double-encoded text.

Usage: python3 scripts/check_encoding.py
"""

from __future__ import annotations

import pathlib
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent

# file -> strings that must appear verbatim
CHECKS: dict[str, list[str]] = {
    "workbuddy/creditrate.go": [
        "估算值：CodeBuddy 不上报单请求积分",
        "可用 credit_rates 配置覆盖每个模型的系数",
    ],
    "workbuddy/panel.html": [
        "积分 ↔ Token 换算",
        "输出占比",
        "仅看 1 积分",
        "缓存读取(命中)",
        "1 积分 ≈ 各模型可换 Token",
    ],
    "workbuddy/README.md": [
        "Credits ↔ tokens conversion",
        "积分 ↔ Token 换算",
    ],
    "workbuddy/README_CN.md": [
        "积分 ↔ Token 费率表",
        "每积分对应的计费 token 数",
        "按模型覆盖计费系数",
    ],
    "workbuddy/CHANGELOG.md": [
        # The CHANGELOG prose is English; only the panel/view names are Chinese.
        "积分 ↔ Token",
        "消耗明细",
        "缓存读取(命中)",
    ],
    "workbuddy/docs/architecture.md": [
        "One rate card answers both directions",
    ],
    "workbuddy/docs/development.md": [
        "积分 ↔ Token",
    ],
}

# Characters that indicate double-encoded (mojibake) text when they appear in a
# run. These are the classic GBK-misread-as-UTF-8 artifacts.
MOJIBAKE_MARKERS = ["鈮", "脳", "鈥", "锛", "鐨", "涓", "鏄"]


def main() -> int:
    problems: list[str] = []
    for rel, needles in CHECKS.items():
        path = REPO / rel
        if not path.is_file():
            problems.append(f"{rel}: file not found")
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError as exc:
            problems.append(f"{rel}: not valid UTF-8 ({exc})")
            continue
        for needle in needles:
            if needle not in text:
                problems.append(f"{rel}: missing {needle!r}")
        for marker in MOJIBAKE_MARKERS:
            if marker in text:
                problems.append(
                    f"{rel}: mojibake marker {marker!r} present "
                    f"(pre-existing OR newly introduced — check the diff)"
                )

    if problems:
        for p in problems:
            print(f"  FAIL {p}")
        return 1
    print(f"encoding OK ({len(CHECKS)} files checked)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
