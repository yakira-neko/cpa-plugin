#!/usr/bin/env bash
# End-to-end test of the REAL shell step from .github/workflows/build.yml
# ("Build FreeBSD shared library"), with curl/tar/sudo/go replaced by mocks.
#
# Why this exists: the previous CI failure was a HARDCODED FreeBSD version
# (14.3-RELEASE) that upstream deleted from the CDN. The fix resolves the
# version at run time, so the parsing and URL construction are the parts most
# likely to rot next. This extracts the workflow's own script (not a copy) and
# drives it, so a future edit that breaks the logic fails here first.
#
# Usage: bash scripts/test-ci-freebsd-step.sh   (from the repo root)
set -uo pipefail

WORKFLOW=".github/workflows/build.yml"
failures=0

command -v node >/dev/null 2>&1 || { echo "SKIP: node not available"; exit 0; }

# --- extract the step's run: block verbatim ---------------------------------
node -e '
const fs=require("fs");
const lines=fs.readFileSync(process.argv[1],"utf8").split(/\r?\n/);
let start=-1,end=lines.length;
for(let i=0;i<lines.length;i++){
  if(/^\s*- name: Build FreeBSD shared library/.test(lines[i])) { start=i; continue; }
  if(start>=0 && /^\s*- name: /.test(lines[i])) { end=i; break; }
}
if(start<0){ console.error("step not found"); process.exit(1); }
const block=lines.slice(start,end);
const runIdx=block.findIndex(l=>/^\s*run: \|/.test(l));
if(runIdx<0){ console.error("run block not found"); process.exit(1); }
const indent=block[runIdx].match(/^(\s*)/)[1].length+2;
// Drop GitHub expression lines we cannot evaluate (${{ ... }}).
const body=block.slice(runIdx+1).map(l=>l.slice(indent)).join("\n")
  .replace(/\$\{\{\s*matrix\.target\s*\}\}/g,"freebsd-amd64");
fs.writeFileSync(process.argv[2], body);
console.log("extracted "+body.split("\n").length+" lines");
' "$WORKFLOW" _step.sh || { echo "FAIL: could not extract step"; exit 1; }

# --- mock environment --------------------------------------------------------
# Mock curl: records the URL, and mirrors real curl semantics — with -o it
# writes to that file, WITHOUT -o it writes to stdout (the workflow captures
# `index=$(curl ...)` from stdout, so this distinction matters).
mkdir -p _mock
cat > _mock/curl <<'EOF'
#!/usr/bin/env bash
url=""
out=""
prev=""
for a in "$@"; do
  case "$a" in
    -o) prev="-o"; continue ;;
  esac
  if [ "$prev" = "-o" ]; then out="$a"; prev=""; continue; fi
  case "$a" in
    -*) ;;
    *) [ -z "$url" ] && url="$a" ;;
  esac
done
echo "$url" >> "$CURL_LOG"
if [ -n "$out" ]; then
  cp "$CURL_FIXTURE" "$out"
else
  # No -o: body goes to stdout (this is what the version resolution reads).
  cat "$CURL_FIXTURE"
fi
exit 0
EOF

# Mock tar/sudo/go: no-ops that record their invocation.
cat > _mock/tar  <<'EOF'
#!/usr/bin/env bash
echo "tar $*" >> "$CMD_LOG"
exit 0
EOF
# Mock sudo: records the call AND runs the rest, so the install branch is
# genuinely exercised rather than silently swallowed. Flags may be passed
# before the command (e.g. `sudo tar -xf ...`), so simply shifting once is
# wrong — find the first non-flag argument.
cat > _mock/sudo <<'EOF'
#!/usr/bin/env bash
echo "sudo $*" >> "$CMD_LOG"
args=("$@")
cmd=""
rest=()
seen_cmd=0
for a in "${args[@]}"; do
  if [ "$seen_cmd" = "0" ]; then
    case "$a" in
      -*) continue ;;         # sudo's own flags
      *) cmd="$a"; seen_cmd=1; continue ;;
    esac
  fi
  rest+=("$a")
done
if [ -z "$cmd" ]; then exit 0; fi
exec "$cmd" "${rest[@]}"
EOF
cat > _mock/go <<'EOF'
#!/usr/bin/env bash
{
  echo "go $*"
  echo "  GOOS=$GOOS GOARCH=$GOARCH CGO_ENABLED=$CGO_ENABLED"
  echo "  CC=$CC"
  echo "  CGO_LDFLAGS=$CGO_LDFLAGS"
} >> "$CMD_LOG"
exit 0
EOF
# ld.lld present -> the apt-get branch must be skipped.
cat > _mock/ld.lld <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x _mock/*

# Fixture: the REAL CDN listing shape (14.3 already pruned upstream).
cat > _mock/index.html <<'EOF'
<html><body>
<a href="../">Parent directory/</a>
<a href="14.4-RELEASE/">14.4-RELEASE/</a>  2026-Mar-06 19:13
<a href="14.5-RELEASE/">14.5-RELEASE/</a>  2026-Sep-04 19:03
<a href="15.0-RELEASE/">15.0-RELEASE/</a>  2025-Nov-28 18:16
<a href="15.1-RELEASE/">15.1-RELEASE/</a>  2026-Jun-12 15:44
<a href="amd64/">amd64/</a>
</body></html>
EOF

run_step() {
  : > "$CURL_LOG"; : > "$CMD_LOG"
  PATH="$PWD/_mock:$PATH" CURL_LOG="$CURL_LOG" CMD_LOG="$CMD_LOG" \
    CURL_FIXTURE="$PWD/_mock/index.html" VERSION="0.9.1" \
    FB_RELEASE="${1:-}" LIB_NAME="workbuddy.so" timeout 60 bash _step.sh >_step.out 2>&1
  step_rc=$?
}

check() {
  local name="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    echo "  ok   $name"
  else
    echo "  FAIL $name"
    echo "         want: $want"
    echo "         got : $got"
    failures=$((failures+1))
  fi
}

check_contains() {
  local name="$1" needle="$2" hay="$3"
  if printf '%s' "$hay" | grep -qF -- "$needle"; then
    echo "  ok   $name"
  else
    echo "  FAIL $name (missing: $needle)"
    failures=$((failures+1))
  fi
}

echo "== case 1: current CDN (14.3 pruned) =="
CURL_LOG="$PWD/_curl.log"; CMD_LOG="$PWD/_cmd.log"
run_step ""
if [ $step_rc -ne 0 ]; then
  echo "  FAIL step exited $step_rc"; cat _step.out; failures=$((failures+1))
else
  index_url=$(grep -c 'releases/amd64/$' _curl.log || true)
  check "fetched the amd64 index" "1" "$index_url"
  check "requested the newest 14.x base.txz" "1" \
        "$(grep -c 'amd64/14.5-RELEASE/base.txz' _curl.log || true)"
  check_contains "logged the resolved release" "Using FreeBSD 14.5-RELEASE" "$(cat _step.out)"
  check_contains "go build ran with GOOS=freebsd" "GOOS=freebsd GOARCH=amd64 CGO_ENABLED=1" "$(cat _cmd.log)"
  check_contains "CC targets freebsd14.5" "--target=x86_64-unknown-freebsd14.5" "$(cat _cmd.log)"
  check_contains "CC points --sysroot at the extracted sysroot" "--sysroot=" "$(cat _cmd.log)"
  check_contains "links with lld" "CGO_LDFLAGS=-fuse-ld=lld" "$(cat _cmd.log)"
  check_contains "passes the version stamp" "-X main.version=0.9.1" "$(cat _cmd.log)"
  check_contains "outputs the pluginstore name" "workbuddy.so" "$(cat _cmd.log)"
  # ld.lld is present in the mock -> must NOT run apt-get.
  check "skipped apt-get when lld exists" "0" "$(grep -c 'apt-get' _cmd.log || true)"
fi

echo "== case 2: only 15.x remains (fallback path) =="
cat > _mock/index.html <<'EOF'
<a href="15.0-RELEASE/">15.0-RELEASE/</a>
<a href="15.1-RELEASE/">15.1-RELEASE/</a>
EOF
run_step ""
if [ $step_rc -ne 0 ]; then
  echo "  FAIL step exited $step_rc"; cat _step.out; failures=$((failures+1))
else
  check "fell back to the newest release" "1" \
        "$(grep -c 'amd64/15.1-RELEASE/base.txz' _curl.log || true)"
  check_contains "go targets freebsd15.1" "--target=x86_64-unknown-freebsd15.1" "$(cat _cmd.log)"
fi

echo "== case 3: explicit FB_RELEASE pin wins =="
run_step "13.2-RELEASE"
if [ $step_rc -ne 0 ]; then
  echo "  FAIL step exited $step_rc"; cat _step.out; failures=$((failures+1))
else
  check "pinned version used" "1" \
        "$(grep -c 'amd64/13.2-RELEASE/base.txz' _curl.log || true)"
  # A pinned version must not even need the index listing.
  check "no index fetch when pinned" "0" "$(grep -c 'releases/amd64/$' _curl.log || true)"
fi

echo "== case 4: empty listing fails loudly (no silent success) =="
: > _mock/index.html
run_step ""
if [ $step_rc -eq 0 ]; then
  echo "  FAIL empty listing should have failed the step"; failures=$((failures+1))
else
  check_contains "emitted a GitHub error annotation" "::error::" "$(cat _step.out)"
fi

echo "== case 5: lld missing -> apt-get installs it =="
# Hide the mocked ld.lld so `command -v` fails, exercising the install branch.
mv _mock/ld.lld _mock/ld.lld.hidden
cat > _mock/apt-get <<'EOF'
#!/usr/bin/env bash
echo "apt-get $*" >> "$CMD_LOG"
exit 0
EOF
chmod +x _mock/apt-get
cat > _mock/index.html <<'EOF'
<a href="14.5-RELEASE/">14.5-RELEASE/</a>
EOF
run_step ""
if [ $step_rc -ne 0 ]; then
  echo "  FAIL step exited $step_rc"; cat _step.out; failures=$((failures+1))
else
  check_contains "installed clang + lld when absent" "apt-get install -y -qq clang lld" "$(cat _cmd.log)"
fi
mv _mock/ld.lld.hidden _mock/ld.lld

# --- cleanup -----------------------------------------------------------------
rm -rf _step.sh _mock _curl.log _cmd.log _step.out

echo ""
if [ "$failures" -eq 0 ]; then
  echo "all FreeBSD CI step checks passed"
  exit 0
fi
echo "$failures check(s) failed"
exit 1
