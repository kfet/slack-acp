#!/bin/sh
# install.sh — download and install the slack-acp binary.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/kfet/slack-acp/main/install.sh | sh
#
# Environment overrides:
#   VERSION   Release tag to install (default: latest). Example: VERSION=v0.1.0
#   BIN_DIR   Install destination (default: /usr/local/bin, falling back to
#             $HOME/.local/bin when /usr/local/bin is not writable).
#   OS        Override detected OS (linux | darwin).
#   ARCH      Override detected arch (amd64 | arm64 | armv6).
#
# Requires: curl (or wget), tar/uname/mktemp, and one of sha256sum/shasum.

set -eu

REPO="kfet/slack-acp"
BIN_NAME="slack-acp"

log()  { printf '==> %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# ---- fetch helper ---------------------------------------------------------
download() {
	# download URL OUTFILE
	if have curl; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif have wget; then
		wget -qO "$2" "$1"
	else
		die "need curl or wget"
	fi
}

# ---- detect platform ------------------------------------------------------
detect_os() {
	case "$(uname -s)" in
		Linux)   echo linux ;;
		Darwin)  echo darwin ;;
		*)       die "unsupported OS: $(uname -s)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
		x86_64|amd64)      echo amd64 ;;
		arm64|aarch64)     echo arm64 ;;
		armv6l|armv6)      echo armv6 ;;
		armv7l|armv7)
			# No armv7 build published; armv6 binary runs on armv7 hardware.
			echo armv6 ;;
		*) die "unsupported arch: $(uname -m)" ;;
	esac
}

OS="${OS:-$(detect_os)}"
ARCH="${ARCH:-$(detect_arch)}"

# darwin/armv6 is not a real target.
if [ "$OS" = darwin ] && [ "$ARCH" = armv6 ]; then
	die "no darwin/armv6 release; use arm64 on Apple Silicon"
fi

# ---- resolve version ------------------------------------------------------
VERSION="${VERSION:-latest}"
if [ "$VERSION" = latest ]; then
	log "resolving latest release for $REPO"
	tmp_json="$(mktemp)"
	trap 'rm -f "$tmp_json"' EXIT
	download "https://api.github.com/repos/$REPO/releases/latest" "$tmp_json"
	# Extract "tag_name": "v..." without jq.
	VERSION="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp_json" | head -n1)"
	rm -f "$tmp_json"; trap - EXIT
	[ -n "$VERSION" ] || die "could not parse latest release tag"
fi
log "installing $REPO $VERSION ($OS/$ARCH)"

# ---- download binary + checksums -----------------------------------------
ASSET="${BIN_NAME}-${OS}-${ARCH}"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

log "downloading $ASSET"
download "$BASE_URL/$ASSET"        "$tmpdir/$ASSET"
download "$BASE_URL/checksums.txt" "$tmpdir/checksums.txt"

# ---- verify checksum ------------------------------------------------------
if have sha256sum;   then SHA="sha256sum"
elif have shasum;    then SHA="shasum -a 256"
else                      die "need sha256sum or shasum"
fi

expected="$(awk -v f="$ASSET" '$2==f {print $1; exit}' "$tmpdir/checksums.txt")"
[ -n "$expected" ] || die "no checksum entry for $ASSET in checksums.txt"
actual="$(cd "$tmpdir" && $SHA "$ASSET" | awk '{print $1}')"
[ "$expected" = "$actual" ] || die "checksum mismatch for $ASSET (expected $expected, got $actual)"
log "checksum ok"

# ---- install --------------------------------------------------------------
if [ -z "${BIN_DIR:-}" ]; then
	if [ -w /usr/local/bin ] 2>/dev/null; then
		BIN_DIR=/usr/local/bin
	elif [ "$(id -u)" = 0 ]; then
		BIN_DIR=/usr/local/bin
	else
		BIN_DIR="$HOME/.local/bin"
	fi
fi
mkdir -p "$BIN_DIR"

dest="$BIN_DIR/$BIN_NAME"
chmod +x "$tmpdir/$ASSET"

if [ -w "$BIN_DIR" ]; then
	mv "$tmpdir/$ASSET" "$dest"
elif have sudo; then
	log "installing to $dest via sudo"
	sudo mv "$tmpdir/$ASSET" "$dest"
else
	die "cannot write to $BIN_DIR (set BIN_DIR=… to override)"
fi

log "installed $dest"
case ":$PATH:" in
	*":$BIN_DIR:"*) ;;
	*) log "note: $BIN_DIR is not on \$PATH" ;;
esac

"$dest" --version 2>/dev/null || true
