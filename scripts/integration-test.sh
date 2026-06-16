#!/usr/bin/env bash
# Build the test image and run the hermetic integration suite with no network.
#
#   scripts/integration-test.sh            # run the suite
#   DOCKER="sudo docker" scripts/integration-test.sh
#
# Extra args are passed through to `go test`, e.g.:
#   scripts/integration-test.sh -run LFSResolved
set -euo pipefail

DOCKER="${DOCKER:-docker}"
IMG="${IMG:-hegel-modproxy-test}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Build needs the network (apk + go mod download); the test run does not.
$DOCKER build --target test -t "$IMG" "$ROOT"
$DOCKER run --rm --network=none "$IMG"
