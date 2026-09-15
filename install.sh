#!/bin/sh
# agentflow installer.
#
#   curl -fsSL https://raw.githubusercontent.com/arthurobo/agentflow/main/install.sh | sh
#
# Downloads the release archive for this OS and CPU, verifies it against the
# release's checksums.txt (and, when cosign is installed, verifies the
# signature on checksums.txt), installs the binary to ~/.local/bin/agentflow
# and runs `agentflow start`, which sets up the background service, this
# machine's remote address and phone pairing.
#
# No Go toolchain is needed. This script registers nothing: the agentflow
# daemon itself asks the account service for this machine's Cloudflare tunnel
# (https://<name>.useagentflow.xyz) when it starts, sending a random machine id,
# the hostname and a secret it keeps. Set AF_REMOTE=tailscale (or off) in
# ~/.config/agentflow/agentflow.env to use Tailscale instead.
#
# Environment:
#   AGENTFLOW_VERSION       release tag to install, e.g. v0.6.0 (default: latest)
#   AGENTFLOW_RELEASE_BASE  base URL that holds the release files directly
#                           (default: the GitHub release download URL)
#   AGENTFLOW_NO_START=1    install only; don't run `agentflow start`

set -eu

REPO="arthurobo/agentflow"
BIN_DIR="${HOME:?HOME must be set}/.local/bin"
CERT_IDENTITY_REGEXP='^https://github\.com/arthurobo/agentflow/\.github/workflows/release\.yml@refs/tags/v.*$'
CERT_OIDC_ISSUER='https://token.actions.githubusercontent.com'

say() { printf 'agentflow: %s\n' "$*"; }
die() {
  printf 'agentflow: error: %s\n' "$*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

need uname
need tar
need mktemp
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --proto '=https,http' --retry 3 -o "$2" "$1"; }
  fetch_stdout() { curl -fsSL --proto '=https,http' --retry 3 "$1"; }
  # api_get honors GITHUB_TOKEN so shared-IP networks can raise the API's
  # 60-request/hour unauthenticated limit to 5000/hour.
  api_get() {
    if [ -n "${GITHUB_TOKEN:-}" ]; then
      curl -fsSL --proto '=https' --retry 3 -H "Authorization: Bearer ${GITHUB_TOKEN}" "$1" 2>/dev/null
    else
      curl -fsSL --proto '=https' --retry 3 "$1" 2>/dev/null
    fi
  }
  # redirect_url follows redirects and prints the final URL, without touching
  # the rate-limited API.
  redirect_url() { curl -fsSL --proto '=https' -o /dev/null -w '%{url_effective}' "$1" 2>/dev/null; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
  fetch_stdout() { wget -q -O - "$1"; }
  api_get() {
    if [ -n "${GITHUB_TOKEN:-}" ]; then
      wget -q -O - --header="Authorization: Bearer ${GITHUB_TOKEN}" "$1" 2>/dev/null
    else
      wget -q -O - "$1" 2>/dev/null
    fi
  }
  redirect_url() { wget -qS --max-redirect=0 -O /dev/null "$1" 2>&1 | sed -n 's/.*[Ll]ocation:[[:space:]]*//p' | tr -d "\r" | tail -n1; }
else
  die "curl or wget is required"
fi

# --- platform ---------------------------------------------------------------
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported operating system: $(uname -s) (release builds exist for Linux and macOS)" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported CPU architecture: $(uname -m) (release builds exist for amd64 and arm64)" ;;
esac

# --- version ----------------------------------------------------------------
version="${AGENTFLOW_VERSION:-}"
if [ -z "$version" ]; then
  say "looking up the latest release"
  api="https://api.github.com/repos/${REPO}/releases/latest"
  version=$(api_get "$api" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
  if [ -z "$version" ]; then
    # The unauthenticated GitHub API allows only 60 requests/hour per IP, which
    # a shared IP (an office NAT, CI) can exhaust and then answer 403. Fall back
    # to the release-page redirect, which is not subject to that limit, so a
    # first install never fails on a busy network. (Set GITHUB_TOKEN to keep
    # using the API at 5000/hour instead.)
    say "GitHub API unavailable (rate limit?); resolving via the release page"
    version=$(redirect_url "https://github.com/${REPO}/releases/latest" |
      sed -n 's#.*/releases/tag/##p' | head -n 1)
  fi
  [ -n "$version" ] ||
    die "could not determine the latest release tag; set AGENTFLOW_VERSION=vX.Y.Z (or GITHUB_TOKEN) and retry"
fi
case "$version" in
  v*) ;;
  *) version="v$version" ;;
esac
number="${version#v}"

base="${AGENTFLOW_RELEASE_BASE:-https://github.com/${REPO}/releases/download/${version}}"
base="${base%/}"
# GoReleaser's default archive name; `agentflow update` uses the same.
asset="agentflow_${number}_${os}_${arch}.tar.gz"

workdir=$(mktemp -d "${TMPDIR:-/tmp}/agentflow-install.XXXXXX")
trap 'rm -rf "$workdir"' EXIT
trap 'exit 130' INT TERM

say "downloading ${asset} (${version})"
fetch "${base}/${asset}" "${workdir}/${asset}" || die "download failed: ${base}/${asset}"
fetch "${base}/checksums.txt" "${workdir}/checksums.txt" || die "download failed: ${base}/checksums.txt"

# --- signature (when cosign is available) -----------------------------------
if command -v cosign >/dev/null 2>&1; then
  say "verifying the signature on checksums.txt with cosign"
  fetch "${base}/checksums.txt.sig" "${workdir}/checksums.txt.sig" ||
    die "cosign is installed but ${base}/checksums.txt.sig could not be downloaded; not installing"
  fetch "${base}/checksums.txt.pem" "${workdir}/checksums.txt.pem" ||
    die "cosign is installed but ${base}/checksums.txt.pem could not be downloaded; not installing"
  cosign verify-blob \
    --certificate "${workdir}/checksums.txt.pem" \
    --signature "${workdir}/checksums.txt.sig" \
    --certificate-identity-regexp "$CERT_IDENTITY_REGEXP" \
    --certificate-oidc-issuer "$CERT_OIDC_ISSUER" \
    "${workdir}/checksums.txt" ||
    die "signature verification failed; not installing"
else
  say "cosign not found; skipping signature verification (checksums are still verified)"
fi

# --- checksum ---------------------------------------------------------------
expected=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "${workdir}/checksums.txt")
[ -n "$expected" ] || die "checksums.txt has no entry for ${asset}"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "${workdir}/${asset}" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "${workdir}/${asset}" | awk '{ print $1 }')
else
  die "sha256sum or shasum is required to verify the download"
fi
[ "$actual" = "$expected" ] || die "checksum mismatch for ${asset} (expected ${expected}, got ${actual}); not installing"
say "checksum verified"

# --- install ----------------------------------------------------------------
tar -xzf "${workdir}/${asset}" -C "$workdir" agentflow || die "${asset} does not contain an agentflow binary"
[ -f "${workdir}/agentflow" ] || die "${asset} does not contain an agentflow binary"
mkdir -p "$BIN_DIR"
# Copy next to the target and rename, so a running binary is never half-written.
cp "${workdir}/agentflow" "${BIN_DIR}/.agentflow.new"
chmod 0755 "${BIN_DIR}/.agentflow.new"
mv -f "${BIN_DIR}/.agentflow.new" "${BIN_DIR}/agentflow"
say "installed ${BIN_DIR}/agentflow"

case ":${PATH:-}:" in
  *":${BIN_DIR}:"*) ;;
  *)
    say "${BIN_DIR} is not on your PATH. Add it, for example:"
    say "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.profile"
    ;;
esac

if [ "${AGENTFLOW_NO_START:-}" = "1" ]; then
  say "AGENTFLOW_NO_START=1: not starting anything. Run 'agentflow start' when you are ready."
  exit 0
fi

say "running 'agentflow start'"
exec "${BIN_DIR}/agentflow" start
