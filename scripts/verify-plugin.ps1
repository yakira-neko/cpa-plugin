# Verify a c-shared cgo plugin package without a C compiler on the host.
#
# Why this exists: workbuddy/qoderwork are `package main` with `import "C"` in
# main.go. `go build` / `go vet` / `go test` therefore all require a C toolchain,
# which this Windows dev box does not have (no gcc/clang; VS ships MSVC `cl` but
# Go's cgo driver wants gcc). Rather than leave the test suite un-runnable, this
# script copies the package to a scratch dir, strips ONLY the cgo surface from
# main.go via scripts/strip_cgo.py, and runs gofmt + go vet + go test there.
#
# Scope: everything except main.go's C ABI layer is exercised for real. CI
# compiles that layer on the ubuntu/macos/windows runners.
#
# Usage:  pwsh -File scripts/verify-plugin.ps1 [-Plugin workbuddy] [-KeepScratch]
#
# The -Race switch is accepted but ignored: -race requires cgo, which needs the
# C compiler this host lacks. CI runs the race detector on real runners.
param(
    [string]$Plugin = "workbuddy",
    [switch]$Race,
    [switch]$KeepScratch
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
$src = Join-Path $repo $Plugin
$strip = Join-Path $PSScriptRoot "strip_cgo.py"
if (-not (Test-Path $src)) { throw "plugin dir not found: $src" }
if (-not (Test-Path $strip)) { throw "strip_cgo.py not found: $strip" }

$python = (Get-Command python -ErrorAction SilentlyContinue).Source
if (-not $python) { $python = (Get-Command python3 -ErrorAction SilentlyContinue).Source }
if (-not $python) { throw "python not found on PATH" }

$scratch = Join-Path ([System.IO.Path]::GetTempPath()) ("dsh-verify-$Plugin-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
Write-Host "[verify] plugin  : $Plugin"
Write-Host "[verify] scratch : $scratch"

& $python $strip $src $scratch
if ($LASTEXITCODE -ne 0) { throw "strip_cgo.py failed ($LASTEXITCODE)" }

# Turn native stderr ErrorRecords into plain strings so string methods work.
function Out-Text {
    param($Lines)
    $Lines | ForEach-Object { [string]$_ } | Where-Object { $_ -and $_.Trim() }
}

Push-Location $scratch
$failed = $false
try {
    $env:GOCACHE = Join-Path $repo ".gocache"
    $env:GOTELEMETRY = "off"
    $env:CGO_ENABLED = "0"
    $env:GOFLAGS = ""

    Write-Host ""
    Write-Host "[verify] gofmt"
    $unformatted = @(Out-Text (& gofmt -l . 2>&1))
    if ($unformatted.Count -gt 0) {
        Write-Host "  UNFORMATTED:"
        $unformatted | ForEach-Object { Write-Host "    $_" }
        $failed = $true
    } else {
        Write-Host "  clean"
    }

    Write-Host ""
    Write-Host "[verify] go vet"
    $vetOut = @(Out-Text (& go vet ./... 2>&1))
    $vetExit = $LASTEXITCODE
    if ($vetOut.Count -gt 0) { $vetOut | ForEach-Object { Write-Host "  $_" } }
    if ($vetExit -ne 0) { $failed = $true; Write-Host "  vet exit: $vetExit" } else { Write-Host "  clean" }

    Write-Host ""
    Write-Host "[verify] go test"
    Write-Host "  (no -race: it requires cgo, which needs a C compiler this host lacks."
    Write-Host "   CI runs the race detector on the Linux/macOS runners.)"
    $testOut = @(Out-Text (& go test -count=1 ./... 2>&1))
    $testExit = $LASTEXITCODE
    $testOut | Select-Object -Last 60 | ForEach-Object { Write-Host "  $_" }
    if ($testExit -ne 0) { $failed = $true }

    Write-Host ""
    Write-Host ("[verify] RESULT: gofmt={0} vet={1} test={2}" -f `
        $(if ($unformatted.Count -gt 0) { "FAIL" } else { "ok" }), `
        $(if ($vetExit -ne 0) { "FAIL" } else { "ok" }), `
        $(if ($testExit -ne 0) { "FAIL" } else { "ok" }))
}
finally {
    Pop-Location
    if ($failed -or $KeepScratch) {
        Write-Host "[verify] scratch kept for inspection: $scratch"
    } else {
        Remove-Item -Recurse -Force $scratch -ErrorAction SilentlyContinue
    }
}
if ($failed) { exit 1 }
exit 0
