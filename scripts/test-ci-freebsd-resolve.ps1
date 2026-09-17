# Verifies the FB_RELEASE resolution logic used by
# .github/workflows/build.yml (build-cross -> "Build FreeBSD shared library"),
# reimplemented in PowerShell because MSYS bash is unavailable in this sandbox.
#
# This guard exists because the ORIGINAL CI failure was a hardcoded FreeBSD
# version (14.3-RELEASE) that upstream deleted from the CDN. The fix resolves the
# version at run time, which makes THIS PARSING the next most likely thing to rot
# -- so it is pinned here against the exact directory shapes the CDN produces.
#
# The PowerShell below mirrors the shell pipeline:
#   grep -oE '[0-9]+\.[0-9]+-RELEASE' | sort -u -V | grep -E '^14\.' | tail -n1
# with the same fallback to the newest release of any major version.

$ErrorActionPreference = 'Stop'
$failures = 0

# Mirrors: sort -u -V (unique, numeric/version order) then tail -n1.
function Resolve-FbRelease {
    param([string]$Index, [string]$Pinned = '')

    if ($Pinned) { return $Pinned }

    $all = [regex]::Matches($Index, '\d+\.\d+-RELEASE') |
        ForEach-Object { $_.Value } | Select-Object -Unique
    if (-not $all) { return '' }

    # sort -V: compare numerically component-wise, so 14.10 > 14.9.
    $sorted = $all | Sort-Object { [version]($_.Replace('-RELEASE', '')) }
    $fourteen = $sorted | Where-Object { $_ -match '^14\.' }
    if ($fourteen) { return ($fourteen | Select-Object -Last 1) }
    return ($sorted | Select-Object -Last 1)
}

function Check {
    param([string]$Name, [string]$Want, [string]$Got)
    if ($Want -eq $Got) {
        Write-Host "  ok   $Name -> '$Got'"
    } else {
        Write-Host "  FAIL $Name -> got '$Got', want '$Want'"
        $script:failures++
    }
}

# --- the CDN as of 2026-09-16 (14.3 already pruned) --------------------------
$cdn2026 = @'
<a href="../">Parent directory/</a>
<a href="14.4-RELEASE/">14.4-RELEASE/</a>  2026-Mar-06 19:13
<a href="14.5-RELEASE/">14.5-RELEASE/</a>  2026-Sep-04 19:03
<a href="15.0-RELEASE/">15.0-RELEASE/</a>  2025-Nov-28 18:16
<a href="15.1-RELEASE/">15.1-RELEASE/</a>  2026-Jun-12 15:44
<a href="amd64/">amd64/</a>
README.TXT
'@
Check "picks newest 14.x from current CDN" '14.5-RELEASE' (Resolve-FbRelease $cdn2026)

# --- markdown table shape (what the web_fetch of the index returns) ----------
$md = @'
| [14.4-RELEASE/](14.4-RELEASE/ "14.4-RELEASE") | -  | 2026-Mar-06 19:13 |
| [14.5-RELEASE/](14.5-RELEASE/ "14.5-RELEASE") | -  | 2026-Sep-04 19:03 |
| [15.0-RELEASE/](15.0-RELEASE/ "15.0-RELEASE") | -  | 2025-Nov-28 18:16 |
'@
Check "parses markdown-table listing" '14.5-RELEASE' (Resolve-FbRelease $md)

# --- the OLD shape that broke CI: 14.3 present ------------------------------
$old = '<a href="14.3-RELEASE/">14.3-RELEASE/</a> <a href="14.4-RELEASE/">14.4-RELEASE/</a>'
Check "would have picked 14.4 back then (14.3 present)" '14.4-RELEASE' (Resolve-FbRelease $old)

# --- 14.x fully pruned: fall back to newest of anything ---------------------
$only15 = '<a href="15.0-RELEASE/">15.0-RELEASE/</a> <a href="15.1-RELEASE/">15.1-RELEASE/</a>'
Check "falls back when no 14.x remains" '15.1-RELEASE' (Resolve-FbRelease $only15)

# --- numeric (not lexical) version sort -------------------------------------
$numeric = '<a href="14.9-RELEASE/">14.9-RELEASE/</a> <a href="14.10-RELEASE/">14.10-RELEASE/</a>'
Check "version-sorts numerically (14.10 > 14.9)" '14.10-RELEASE' (Resolve-FbRelease $numeric)

# --- empty listing yields empty (caller then errors out loudly) -------------
Check "empty listing yields empty" '' (Resolve-FbRelease '')

# --- unrelated numeric text must not be mistaken for a release --------------
$noise = '2026-Mar-06 19:13 amd64 README.TXT BuildID=14.5 build=20260916'
Check "ignores non-release text" '' (Resolve-FbRelease $noise)

# --- explicit pin (FB_RELEASE) always wins ----------------------------------
Check "explicit pin overrides detection" '13.2-RELEASE' (Resolve-FbRelease $cdn2026 '13.2-RELEASE')

Write-Host ""
if ($failures -eq 0) {
    Write-Host "all FB_RELEASE resolution checks passed"
    exit 0
}
Write-Host "$failures check(s) failed"
exit 1
