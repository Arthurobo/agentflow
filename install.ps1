# agentflow installer for Windows (PowerShell).
#
#   irm https://raw.githubusercontent.com/arthurobo/agentflow/main/install.ps1 | iex
#
# Downloads the release zip for Windows, verifies it against the release's
# checksums.txt (and, when cosign is installed, the signature on checksums.txt),
# installs agentflow.exe to %LOCALAPPDATA%\agentflow\bin, adds it to your user
# PATH, and runs `agentflow start` — which sets up the background task, this
# machine's Cloudflare tunnel (https://<name>.useagentflow.xyz) and phone
# pairing.
#
# No Go toolchain is needed. Environment:
#   AGENTFLOW_VERSION       release tag to install, e.g. v0.6.0 (default: latest)
#   AGENTFLOW_RELEASE_BASE  base URL holding the release files directly
#   AGENTFLOW_NO_START=1    install only; don't run `agentflow start`

$ErrorActionPreference = 'Stop'
# Ensure TLS 1.2 on Windows PowerShell 5.1 (best-effort; newer PowerShell already
# negotiates it). The correct type is SecurityProtocolType, not SecurityProtocol.
try {
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { }

$Repo = 'arthurobo/agentflow'
$BinDir = Join-Path $env:LOCALAPPDATA 'agentflow\bin'
$CertIdentityRegexp = '^https://github\.com/arthurobo/agentflow/\.github/workflows/release\.yml@refs/tags/v.*$'
$CertOidcIssuer = 'https://token.actions.githubusercontent.com'

function Say($m) { Write-Host "agentflow: $m" }
function Die($m) { Write-Error "agentflow: error: $m"; exit 1 }

# --- platform ---------------------------------------------------------------
# Only windows/amd64 is published; Windows on ARM runs it under emulation.
$os = 'windows'
$arch = 'amd64'

# --- version ----------------------------------------------------------------
$version = $env:AGENTFLOW_VERSION
if (-not $version) {
  Say 'looking up the latest release'
  try {
    $headers = @{ 'User-Agent' = 'agentflow-install' }
    if ($env:GITHUB_TOKEN) { $headers['Authorization'] = "Bearer $($env:GITHUB_TOKEN)" }
    $rel = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/$Repo/releases/latest"
    $version = $rel.tag_name
  } catch {
    Say 'GitHub API unavailable (rate limit?); resolving via the release page'
    try {
      $resp = Invoke-WebRequest -MaximumRedirection 0 -ErrorAction SilentlyContinue -Uri "https://github.com/$Repo/releases/latest"
      $loc = $resp.Headers.Location
      if ($loc) { $version = ($loc -split '/tag/')[-1] }
    } catch {
      if ($_.Exception.Response.Headers.Location) {
        $version = ($_.Exception.Response.Headers.Location.ToString() -split '/tag/')[-1]
      }
    }
  }
}
if (-not $version) { Die 'could not determine the latest release tag; set $env:AGENTFLOW_VERSION=vX.Y.Z and retry' }
if ($version -notlike 'v*') { $version = "v$version" }
$number = $version.TrimStart('v')

$base = $env:AGENTFLOW_RELEASE_BASE
if (-not $base) { $base = "https://github.com/$Repo/releases/download/$version" }
$base = $base.TrimEnd('/')
$asset = "agentflow_${number}_${os}_${arch}.zip"

$workdir = Join-Path ([IO.Path]::GetTempPath()) ("agentflow-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $workdir | Out-Null
try {
  Say "downloading $asset ($version)"
  Invoke-WebRequest -Uri "$base/$asset" -OutFile (Join-Path $workdir $asset)
  Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile (Join-Path $workdir 'checksums.txt')

  # --- signature (when cosign is available) ---------------------------------
  if (Get-Command cosign -ErrorAction SilentlyContinue) {
    Say 'verifying the signature on checksums.txt with cosign'
    Invoke-WebRequest -Uri "$base/checksums.txt.sig" -OutFile (Join-Path $workdir 'checksums.txt.sig')
    Invoke-WebRequest -Uri "$base/checksums.txt.pem" -OutFile (Join-Path $workdir 'checksums.txt.pem')
    & cosign verify-blob `
      --certificate (Join-Path $workdir 'checksums.txt.pem') `
      --signature (Join-Path $workdir 'checksums.txt.sig') `
      --certificate-identity-regexp $CertIdentityRegexp `
      --certificate-oidc-issuer $CertOidcIssuer `
      (Join-Path $workdir 'checksums.txt')
    if ($LASTEXITCODE -ne 0) { Die 'signature verification failed; not installing' }
  } else {
    Say 'cosign not found; skipping signature verification (checksums are still verified)'
  }

  # --- checksum -------------------------------------------------------------
  $line = Select-String -Path (Join-Path $workdir 'checksums.txt') -Pattern ([regex]::Escape($asset)) | Select-Object -First 1
  if (-not $line) { Die "checksums.txt has no entry for $asset" }
  $expected = ($line.Line -split '\s+')[0].ToLower()
  $actual = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $workdir $asset)).Hash.ToLower()
  if ($actual -ne $expected) { Die "checksum mismatch for $asset (expected $expected, got $actual); not installing" }
  Say 'checksum verified'

  # --- install --------------------------------------------------------------
  Expand-Archive -Force -Path (Join-Path $workdir $asset) -DestinationPath (Join-Path $workdir 'extracted')
  $exe = Join-Path $workdir 'extracted\agentflow.exe'
  if (-not (Test-Path $exe)) { Die "$asset does not contain agentflow.exe" }
  New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
  # Windows can't overwrite a running .exe; move an old one aside first.
  $dest = Join-Path $BinDir 'agentflow.exe'
  if (Test-Path $dest) {
    $old = "$dest.old"
    Remove-Item -Force -ErrorAction SilentlyContinue $old
    try { Move-Item -Force $dest $old } catch { }
  }
  Copy-Item -Force $exe $dest
  Say "installed $dest"

  # --- PATH -----------------------------------------------------------------
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (-not $userPath) { $userPath = '' }
  if (($userPath -split ';') -notcontains $BinDir) {
    $newPath = if ($userPath) { $userPath.TrimEnd(';') + ';' + $BinDir } else { $BinDir }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    $env:Path = $env:Path + ';' + $BinDir
    Say "added $BinDir to your user PATH (restart terminals to pick it up)"
  }

  if ($env:AGENTFLOW_NO_START -eq '1') {
    Say "AGENTFLOW_NO_START=1: not starting. Run 'agentflow start' when you are ready."
    exit 0
  }

  Say "running 'agentflow start'"
  & $dest start
} finally {
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $workdir
}
