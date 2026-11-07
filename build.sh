#!/bin/bash
#
# Build the Cloudflare Tunnel .fpk package for fnOS.
#
#   ./build.sh              regenerate icons, validate, then pack
#   ./build.sh --no-icons   skip icon regeneration
#   ./build.sh --no-pack    run every check, stop before fnpack
#
# This is a thin wrapper. The pipeline itself -- fetch the pinned cloudflared,
# cross-compile the service binary, icons, permission normalisation, the lints,
# fnpack, archive normalisation, artefact verification -- lives in tools/build
# (Go), so it exists once instead of once per host.
#
# Requires Go >= 1.22 and, for the packing step, the official `fnpack` utility
# (https://developer.fnnas.com/docs/cli/fnpack). Set FNPACK when it is not on
# PATH; tools/build resolves it.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

# ------------------------------------------------------------------- locate Go
# PATH wins. The checkout's local toolchain is the fallback, so a working tree
# with .toolchain/ builds without touching the shell profile.
GO_BIN="${GO:-}"
if [ -z "${GO_BIN}" ]; then
    if command -v go >/dev/null 2>&1; then
        GO_BIN="$(command -v go)"
    elif [ -x "${ROOT}/.toolchain/go/bin/go" ]; then
        GO_BIN="${ROOT}/.toolchain/go/bin/go"
    else
        echo "ERROR: go not found in PATH (install Go >= 1.22 or set GO=/path/to/go)" >&2
        exit 1
    fi
fi
if [ ! -x "${GO_BIN}" ]; then
    echo "ERROR: not an executable: ${GO_BIN}" >&2
    exit 1
fi

# `go run` needs a writable cache. Keep it inside the checkout when the local
# toolchain layout is present, so a build does not scatter files through $HOME.
if [ -d "${ROOT}/.toolchain" ]; then
    export GOCACHE="${GOCACHE:-${ROOT}/.toolchain/cache}"
    export GOMODCACHE="${GOMODCACHE:-${ROOT}/.toolchain/mod}"
    export GOTMPDIR="${GOTMPDIR:-${ROOT}/.toolchain/tmp}"
    mkdir -p "${GOCACHE}" "${GOMODCACHE}" "${GOTMPDIR}"
fi

# tools/build shells out to `go` by name when it compiles the service binary.
export PATH="$(dirname "${GO_BIN}"):${PATH}"

exec "${GO_BIN}" run ./tools/build "$@"
