#!/usr/bin/env pwsh
# fetch-reducer.ps1 - native-Windows counterpart of fetch-reducer.sh.
#
# Prints the verified reducer.exe path to stdout; diagnostics go to stderr.
# On ANY error - including a checksum mismatch - it throws and leaves nothing
# executable in the cache. An unverified binary is NEVER run.
#
# Version resolution (first wins): -Version, $env:REDUCER_VERSION, plugin.json.
# Repo override for testing: $env:LASSO_SHOPIFY_TOOLS_REPO
#   (default: subhubapps/lasso-shopify-tools).
#
# The download tag is "v<version>" and the asset name is reducer_windows_<arch>.exe,
# which MUST match .goreleaser.yaml's archives.name_template exactly.
[CmdletBinding()]
param([string]$Version)

$ErrorActionPreference = 'Stop'
function Log($m) { [Console]::Error.WriteLine($m) }

# --- locate plugin root + resolve the version ----------------------------
$pluginJson = Join-Path $PSScriptRoot '..\.claude-plugin\plugin.json'
if (-not $Version) { $Version = $env:REDUCER_VERSION }
if (-not $Version) {
  if (-not (Test-Path $pluginJson)) { throw "fetch-reducer: cannot find plugin.json at $pluginJson; pass -Version or set REDUCER_VERSION" }
  $Version = (Get-Content -Raw $pluginJson | ConvertFrom-Json).version
}
if (-not $Version) { throw "fetch-reducer: could not resolve the plugin version" }

$repo = if ($env:LASSO_SHOPIFY_TOOLS_REPO) { $env:LASSO_SHOPIFY_TOOLS_REPO } else { 'subhubapps/lasso-shopify-tools' }
$tag  = "v$Version"

# --- detect arch (OS is windows on this script) --------------------------
switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { $goarch = 'amd64' }
  'ARM64' { $goarch = 'arm64' }
  default { throw "fetch-reducer: unsupported CPU arch '$($env:PROCESSOR_ARCHITECTURE)' (need AMD64 or ARM64)" }
}

# Asset scheme - keep in lockstep with .goreleaser.yaml (reducer_windows_<arch>.exe):
$asset = "reducer_windows_$goarch.exe"

$cacheDir  = Join-Path $HOME ".cache\lasso-shopify-tools\$Version"
$cachedBin = Join-Path $cacheDir 'reducer.exe'
$cachedSum = Join-Path $cacheDir 'reducer.sha256'

# --- cache hit: reuse offline (re-verify locally when a sum is stored) ----
if (Test-Path $cachedBin) {
  if (Test-Path $cachedSum) {
    $want = ((Get-Content -Raw $cachedSum) -split '\s+')[0].Trim().ToLower()
    $got  = (Get-FileHash -Algorithm SHA256 $cachedBin).Hash.ToLower()
    if ($want -and $want -eq $got) { Log "reducer $Version cached and verified: $cachedBin"; Write-Output $cachedBin; exit 0 }
    Log "cached reducer failed local re-verification; refetching"
    Remove-Item -Force $cachedBin, $cachedSum -ErrorAction SilentlyContinue
  } else {
    Log "reducer $Version cached: $cachedBin"; Write-Output $cachedBin; exit 0
  }
}

# --- download + verify BEFORE first exec ---------------------------------
$baseUrl = "https://github.com/$repo/releases/download/$tag"
$tmp = New-Item -ItemType Directory -Path (Join-Path ([System.IO.Path]::GetTempPath()) ("lasso-reducer-" + [System.Guid]::NewGuid().ToString('N')))
try {
  Log "downloading reducer $Version ($asset) from $repo release $tag ..."
  $assetPath = Join-Path $tmp $asset
  $sumsPath  = Join-Path $tmp 'SHA256SUMS'
  Invoke-WebRequest -Uri "$baseUrl/$asset" -OutFile $assetPath -UseBasicParsing
  Invoke-WebRequest -Uri "$baseUrl/SHA256SUMS" -OutFile $sumsPath -UseBasicParsing

  $expected = $null
  foreach ($line in Get-Content $sumsPath) {
    $parts = $line -split '\s+', 2
    if ($parts.Count -eq 2 -and $parts[1].Trim() -eq $asset) { $expected = $parts[0].Trim().ToLower(); break }
  }
  if (-not $expected) { throw "fetch-reducer: no checksum for $asset in SHA256SUMS (asset-name mismatch?)" }

  $actual = (Get-FileHash -Algorithm SHA256 $assetPath).Hash.ToLower()
  if ($expected -ne $actual) {
    Remove-Item -Force $assetPath -ErrorAction SilentlyContinue
    throw "fetch-reducer: SHA256 mismatch for ${asset}: expected $expected, got $actual - refusing to execute an unverified binary"
  }

  New-Item -ItemType Directory -Force -Path $cacheDir | Out-Null
  Move-Item -Force $assetPath $cachedBin
  "$expected  reducer.exe" | Set-Content -NoNewline $cachedSum
  Log "reducer $Version verified and cached: $cachedBin"
  Write-Output $cachedBin
}
finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
