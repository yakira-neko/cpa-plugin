#!/usr/bin/env python3
"""Strip the cgo layer out of a CPA plugin's main.go so it can be type-checked
and unit-tested on hosts without a C toolchain.

Why this exists
---------------
workbuddy/qoderwork are `package main` with `import "C"` in main.go. `go build`,
`go vet` and `go test` therefore all require a C compiler. This Windows dev box
has none (no gcc/clang; VS ships MSVC `cl` but Go's cgo driver wants gcc), so the
suite would otherwise be un-runnable locally.

Approach
--------
Copy the REAL main.go and delete only its cgo surface:
  * the cgo preamble + `import "C"`
  * the four `//export`ed C ABI functions
  * `writeResponse` (takes *C.cliproxy_buffer)
  * the `hostAPI *C.cliproxy_host_api` declaration (replaced by a Go stub type)
  * hostCall's C function-pointer body (replaced by the test hook + an error)

Everything else — registration, handleMethod dispatch, parseStored, Region
routing, header builders, auth data mapping — is preserved verbatim, so tests
exercise the real code rather than a paraphrase of it.

The transform is strict: every anchor must match exactly once, else this script
exits non-zero. That prevents a silent drift where main.go changes shape and the
harness quietly stops checking the part that changed.

What is NOT covered
-------------------
The C ABI layer itself (cliproxy_plugin_init / cliproxyPluginCall /
cliproxyPluginFree / cliproxyPluginShutdown) and hostCall's real cgo path. CI
compiles those for real on the ubuntu/macos/windows runners via
`go build -buildmode=c-shared`.

Usage
-----
    python3 scripts/strip_cgo.py <plugin-dir> <out-dir>
"""

from __future__ import annotations

import re
import shutil
import sys
from pathlib import Path

# --- anchors: each must appear exactly once -----------------------------------

PREAMBLE_RE = re.compile(
    r"/\*\n.*?\*/\nimport \"C\"\n",
    re.DOTALL,
)

EXPORTED_FUNCS = [
    "cliproxy_plugin_init",
    "cliproxyPluginCall",
    "cliproxyPluginFree",
    "cliproxyPluginShutdown",
]


def _find_func_span(src: str, name: str) -> tuple[int, int]:
    """Return [start, end) of `func <name>(...) <ret> { ... }` via brace matching.

    Handles the `//export` comment line directly above the function too.
    """
    m = re.search(rf"(?m)^(//export[^\n]*\n)?func {re.escape(name)}\(", src)
    if not m:
        raise SystemExit(f"strip_cgo: anchor not found: func {name}")
    start = m.start()
    brace = src.index("{", m.end() - 1)
    depth = 0
    for i in range(brace, len(src)):
        ch = src[i]
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                end = i + 1
                # swallow the trailing newline
                while end < len(src) and src[end] == "\n":
                    end += 1
                return start, end
    raise SystemExit(f"strip_cgo: unbalanced braces in func {name}")


def _replace_once(src: str, old: str, new: str, label: str) -> str:
    n = src.count(old)
    if n != 1:
        raise SystemExit(f"strip_cgo: anchor {label!r} matched {n} times, want 1")
    return src.replace(old, new)


def strip_main(src: str) -> str:
    # 1. cgo preamble + import "C"
    src, n = PREAMBLE_RE.subn("", src, count=1)
    if n != 1:
        raise SystemExit("strip_cgo: cgo preamble not found")

    # 2. the four exported C functions
    for name in EXPORTED_FUNCS:
        start, end = _find_func_span(src, name)
        src = src[:start] + src[end:]

    # 3. writeResponse(*C.cliproxy_buffer, []byte)
    start, end = _find_func_span(src, "writeResponse")
    src = src[:start] + src[end:]

    # 4. hostAPI declaration -> cgo-free stub type.
    src = _replace_once(
        src,
        "\thostAPI        *C.cliproxy_host_api // captured at init, used for async host calls\n",
        "\thostAPI        *cgoHostAPIStub // cgo-free stand-in (see strip_cgo.py)\n",
        "hostAPI declaration",
    )
    src = _replace_once(
        src,
        "var (\n\thostAPI        *cgoHostAPIStub // cgo-free stand-in (see strip_cgo.py)\n",
        "// cgoHostAPIStub mirrors the shape host_bridge.go probes. Always nil here,\n"
        "// so hostBridgeAvailable() reports false and the direct HTTP fallback is\n"
        "// used — the same path unit tests already take.\n"
        "type cgoHostAPIStub struct {\n"
        "\tcall any\n"
        "\tfree any\n"
        "}\n\n"
        "var (\n\thostAPI        *cgoHostAPIStub // cgo-free stand-in (see strip_cgo.py)\n",
        "cgoHostAPIStub type",
    )

    # 5. hostCall: replace the whole function with the hook + error path.
    start, end = _find_func_span(src, "hostCall")
    src = src[:start] + (
        "// hostCall: the cgo function-pointer path is removed by strip_cgo.py.\n"
        "// Production behaviour is covered by CI's real c-shared build; here the\n"
        "// test hook is the only way to answer a host RPC, matching what the\n"
        "// suite's own tests expect.\n"
        "func hostCall(method string, request []byte) ([]byte, error) {\n"
        "\tif hostCallHook != nil {\n"
        "\t\treturn hostCallHook(method, request)\n"
        "\t}\n"
        "\treturn nil, fmt.Errorf(\"host API unavailable\")\n"
        "}\n"
    ) + src[end:]

    # 6. drop the now-unused "unsafe" import.
    src = _replace_once(src, '\t"unsafe"\n', "", "unsafe import")

    if "C." in src:
        leftover = [l for l in src.splitlines() if "C." in l][:5]
        raise SystemExit("strip_cgo: cgo references remain:\n  " + "\n  ".join(leftover))

    return src


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 2
    plugin_dir, out_dir = Path(sys.argv[1]), Path(sys.argv[2])
    if not plugin_dir.is_dir():
        raise SystemExit(f"strip_cgo: not a directory: {plugin_dir}")

    out_dir.mkdir(parents=True, exist_ok=True)

    for f in plugin_dir.glob("*.go"):
        if f.name == "main.go":
            continue
        shutil.copyfile(f, out_dir / f.name)
    for name in ("go.mod", "go.sum", "panel.html", "baseprompt.json"):
        p = plugin_dir / name
        if p.is_file():
            shutil.copyfile(p, out_dir / name)

    stripped = strip_main((plugin_dir / "main.go").read_text(encoding="utf-8"))
    (out_dir / "main.go").write_text(stripped, encoding="utf-8", newline="\n")

    # Normalize CRLF that Windows checkouts (core.autocrlf=true) introduce.
    # gofmt always emits LF and would otherwise flag every file as unformatted.
    # Read with newline="" so the raw CRLF survives to be replaced — the default
    # universal-newline translation would hide it and silently skip the rewrite.
    for f in out_dir.iterdir():
        if f.suffix in (".go", ".mod", ".sum"):
            with open(f, "r", encoding="utf-8", errors="surrogateescape", newline="") as fh:
                text = fh.read()
            with open(f, "w", encoding="utf-8", errors="surrogateescape", newline="") as fh:
                fh.write(text.replace("\r\n", "\n"))

    # gofmt the one file we generated: the deletions/insertions above leave
    # cosmetic whitespace (blank-line and comment-alignment shifts) that says
    # nothing about the source. The other files are copied verbatim, so they
    # stay under gofmt's judgment — that is where a real formatting defect in
    # the plugin would show up.
    import subprocess

    gen = out_dir / "main.go"
    try:
        subprocess.run(["gofmt", "-w", str(gen)], check=True)
    except (OSError, subprocess.CalledProcessError) as exc:
        raise SystemExit(f"strip_cgo: gofmt failed on generated main.go: {exc}")

    print(f"strip_cgo: wrote cgo-free copy of {plugin_dir.name} -> {out_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
