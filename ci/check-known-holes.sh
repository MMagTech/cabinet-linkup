#!/bin/sh
# Known holes in what ships: govulncheck on this code, with the Go that the image's program was built
# with. setup-go takes the newest Go for go.mod's "go 1.27", so a check with that Go alone passed while
# the Dockerfile's Go 1.27.3 had GO-2026-6859 (MMagTech/cabinet-linkup#20).
#
#   ci/check-known-holes.sh <image> [docker|podman]
#
# It reads the code, not the program: govulncheck's -mode binary also counts an advisory on a whole
# package as found when the program holds none of it, as with GO-2026-5932 (golang.org/x/crypto/openpgp).
set -eu

image=$1
engine=${2:-docker}
work=$(mktemp -d)
id=""

cleanup() {
    if [ -n "$id" ]; then $engine rm "$id" >/dev/null 2>&1 || true; fi
    rm -rf "$work"
}
trap cleanup EXIT

id=$($engine create "$image")
$engine cp "$id:/cabinet-linkup" "$work/cabinet-linkup"
built=$(go version "$work/cabinet-linkup" | awk '{print $2}')
case "$built" in
    go1.*) ;;
    *) echo "FAILED: no Go version in the image's program" >&2; exit 1 ;;
esac
echo "the image's program was built with $built"

GOTOOLCHAIN=$built go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
