#!/usr/bin/env sh
# avairy installer — detects your OS/arch, downloads the matching release archive, verifies its
# checksum, and installs the single `avairy` binary (core, node, tui, and auth are subcommands).
#
#   curl -fsSL https://raw.githubusercontent.com/noctarius/avairy/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- v1.0.0      # pick a version (note: sh -s --, not sh --)
#
# Args:
#   [VERSION]            release tag to install, e.g. v1.0.0 or v1.0.0-rc1 (default: latest stable)
# Environment overrides:
#   AVAIRY_VERSION       same as the VERSION arg (the arg wins if both are given)
#   AVAIRY_INSTALL_DIR   where to put the binaries (default: /usr/local/bin if writable, else ~/.local/bin)
#   AVAIRY_REPO          owner/repo to install from (default: noctarius/avairy)
set -eu

REPO="${AVAIRY_REPO:-noctarius/avairy}"

err() { echo "avairy install: $*" >&2; exit 1; }

case "${1:-}" in
	-h|--help)
		echo "usage: install.sh [VERSION]      e.g. v1.0.0, v1.0.0-rc1 (default: latest stable)"
		echo "piped:  curl -fsSL .../install.sh | sh -s -- v1.0.0"
		exit 0 ;;
esac

# --- detect platform -------------------------------------------------------
os=$(uname -s)
arch=$(uname -m)
case "$os" in
	Linux)   OS=linux ;;
	Darwin)  OS=darwin ;;
	FreeBSD) OS=freebsd ;;
	*) err "unsupported OS '$os' (this script covers Linux, macOS, FreeBSD; on Windows download the .zip from the releases page)" ;;
esac
case "$arch" in
	x86_64|amd64)  ARCH=amd64 ;;
	arm64|aarch64) ARCH=arm64 ;;
	*) err "unsupported architecture '$arch'" ;;
esac

command -v curl >/dev/null 2>&1 || err "curl is required"
command -v tar  >/dev/null 2>&1 || err "tar is required"

# --- resolve version -------------------------------------------------------
# Precedence: positional arg ($1) > $AVAIRY_VERSION > latest stable release.
VERSION="${1:-${AVAIRY_VERSION:-}}"
if [ -z "$VERSION" ]; then
	VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
		| sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
	[ -n "$VERSION" ] || err "could not determine the latest release — set AVAIRY_VERSION explicitly"
fi

ASSET="avairy_${VERSION}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"

# --- choose an install dir -------------------------------------------------
BIN="${AVAIRY_INSTALL_DIR:-}"
if [ -z "$BIN" ]; then
	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then BIN=/usr/local/bin; else BIN="$HOME/.local/bin"; fi
fi
mkdir -p "$BIN" || err "cannot create install dir $BIN"

# --- download + verify + install ------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "avairy $VERSION → $OS/$ARCH"
echo "downloading $ASSET ..."
curl -fSL "$BASE/$ASSET" -o "$tmp/$ASSET" || err "download failed: $BASE/$ASSET"

if curl -fsSL "$BASE/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
	echo "verifying checksum ..."
	line=$(grep "  $ASSET\$" "$tmp/SHA256SUMS" || true)
	[ -n "$line" ] || err "no checksum entry for $ASSET"
	( cd "$tmp" && echo "$line" | { sha256sum -c - 2>/dev/null || shasum -a 256 -c -; } ) || err "checksum verification failed"
else
	echo "warning: SHA256SUMS not found — skipping checksum verification" >&2
fi

tar -xzf "$tmp/$ASSET" -C "$tmp"
[ -f "$tmp/avairy" ] || err "archive missing avairy"
mv -f "$tmp/avairy" "$BIN/avairy"
chmod 0755 "$BIN/avairy"

echo "installed: avairy → $BIN"
case ":$PATH:" in
	*":$BIN:"*) ;;
	*) echo "note: $BIN is not on your PATH — add it, e.g.  export PATH=\"$BIN:\$PATH\"" ;;
esac
"$BIN/avairy" version 2>/dev/null || true
