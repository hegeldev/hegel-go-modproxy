#!/usr/bin/env bash
# Smoke-test a deployed modproxy with a real go toolchain, safely.
#
#   scripts/smoke-test.sh                       # test the fly.dev deployment
#   PROXY=http://localhost:3000 scripts/smoke-test.sh
#   scripts/smoke-test.sh hegel.dev/go/hegel@v0.1.0
#
# A module argument is required (it must include an @version, e.g. @latest):
# given an explicit module@version, `go mod download` runs outside any module,
# so there is no fake go.mod to create.
#
# Isolation guarantees (nothing on your machine is touched):
#   - GOMODCACHE points at a throwaway dir, removed on exit.
#   - GONOSUMDB excludes the vanity path from the public checksum database,
#     so the private module path is never published to the transparency log.
#     (We deliberately do NOT use GOPRIVATE: it also sets GONOPROXY, which
#     would bypass the very proxy we are testing.)
set -euo pipefail

PROXY="${PROXY:-https://hegel-go-modproxy.fly.dev}"
MODULE="${1:-hegel.dev/go/hegel@latest}"

export GOMODCACHE="$(mktemp -d)"
trap 'go clean -modcache 2>/dev/null || true; rm -rf "$GOMODCACHE"' EXIT

echo "Proxy:     $PROXY"
echo "Module:    $MODULE"
echo "GOMODCACHE:$GOMODCACHE"
echo

# Fallback list: the vanity module is served by our proxy; transitive public
# deps 404 there and fall through to the public proxy. The proxy must return
# 404 (not 500) for modules it does not serve for this fall-through to work.
#
# go mod download fetches the module (and its deps) straight into GOMODCACHE
# without touching go.mod, and -json reports the resolved version and paths.
GOPROXY="$PROXY,https://proxy.golang.org,direct" \
  go mod download -x -json "$MODULE"
