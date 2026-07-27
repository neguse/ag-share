#!/bin/sh
# Platform dispatch for ag-share hooks: resolve/download the binary and exec it.
# Contract: must NEVER break the agent session — every failure path logs to
# error.log and exits 0. Exit codes of the binary itself pass through exec
# (the toggle block relies on exit 2 reaching the agent).
set -u

BASE="${AG_SHARE_HOME:-${XDG_CONFIG_HOME:-$HOME/.config}/ag-share}"
LOG="$BASE/error.log"

log() {
  mkdir -p "$BASE" 2>/dev/null || true
  printf '%s run.sh: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >>"$LOG" 2>/dev/null || true
}

# Development override: point AG_SHARE_BIN at a locally built binary.
if [ -n "${AG_SHARE_BIN:-}" ] && [ -x "$AG_SHARE_BIN" ]; then
  exec "$AG_SHARE_BIN" "$@"
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)" || { log "cannot resolve plugin root"; exit 0; }
VERSION="$(cat "$ROOT/bin/VERSION" 2>/dev/null | tr -d ' \r\n')"
if [ -z "$VERSION" ] || [ "$VERSION" = "unreleased" ]; then
  log "no released version pinned (bin/VERSION); set AG_SHARE_BIN for development"
  exit 0
fi

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  MINGW* | MSYS* | CYGWIN*) os=windows ;;
  *) log "unsupported OS: $(uname -s)"; exit 0 ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) log "unsupported arch: $(uname -m)"; exit 0 ;;
esac
ext=""
[ "$os" = "windows" ] && ext=".exe"
asset="ag-share-${os}-${arch}${ext}"
bin="$BASE/bin/$VERSION/$asset"

if [ ! -x "$bin" ]; then
  mkdir -p "$(dirname "$bin")" 2>/dev/null || { log "cannot create $(dirname "$bin")"; exit 0; }
  url="https://github.com/neguse/ag-share/releases/download/$VERSION/$asset"
  tmp="$bin.tmp.$$"
  if ! curl -fsSL --max-time 60 -o "$tmp" "$url" 2>/dev/null; then
    log "download failed: $url"
    rm -f "$tmp" 2>/dev/null
    exit 0
  fi
  want="$(awk -v a="$asset" '$2 == a { print $1 }' "$ROOT/bin/checksums.txt" 2>/dev/null)"
  if [ -z "$want" ]; then
    log "no checksum for $asset in bin/checksums.txt"
    rm -f "$tmp" 2>/dev/null
    exit 0
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    got="$(sha256sum "$tmp" | awk '{ print $1 }')"
  elif command -v shasum >/dev/null 2>&1; then
    got="$(shasum -a 256 "$tmp" | awk '{ print $1 }')"
  else
    log "no sha256 tool available"
    rm -f "$tmp" 2>/dev/null
    exit 0
  fi
  if [ "$want" != "$got" ]; then
    log "checksum mismatch for $asset: want $want got $got"
    rm -f "$tmp" 2>/dev/null
    exit 0
  fi
  chmod +x "$tmp" 2>/dev/null
  mv "$tmp" "$bin" 2>/dev/null || { log "cannot move binary into place"; rm -f "$tmp" 2>/dev/null; exit 0; }
fi

exec "$bin" "$@"
