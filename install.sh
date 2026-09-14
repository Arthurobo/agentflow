#!/bin/sh
# agentflow installer.
#
#   curl -fsSL https://raw.githubusercontent.com/arthurobo/agentflow/main/install.sh | sh
#
# Downloads the release archive for this OS and CPU, verifies it against the
# release's checksums.txt (and, when cosign is installed, verifies the
# signature on checksums.txt), installs the binary to ~/.local/bin/agentflow
# and runs `agentflow start`, which sets up the background service, Tailscale
# sign-in and phone pairing.
#
# No Go toolchain is needed and nothing is registered anywhere.
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
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
  fetch_stdout() { wget -q -O - "$1"; }
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
  version=$(fetch_stdout "$api" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1) ||
    die "could not reach $api"
  [ -n "$version" ] || die "could not find the latest release tag at $api"
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
