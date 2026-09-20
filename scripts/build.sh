#!/usr/bin/env bash
# Builds the plugin as a Linux c-shared library inside a throwaway Go container,
# so the deploy host only needs the finished artifact.
#
# Usage:
#   scripts/build.sh                         # linux/amd64
#   TARGETARCH=arm64 scripts/build.sh        # linux/arm64
#   TARGETARCH=arm64 scripts/build.sh <output-dir>
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TARGETARCH="${TARGETARCH:-amd64}"
case "$TARGETARCH" in
	amd64|arm64) ;;
	*)
		echo "unsupported TARGETARCH: $TARGETARCH (expected amd64 or arm64)" >&2
		exit 2
		;;
esac
OUT_DIR="${1:-$REPO_DIR/build/linux/$TARGETARCH}"
GO_IMAGE="${GO_IMAGE:-golang:1.26}"
CACHE_DIR="$REPO_DIR/.gocache"

mkdir -p "$OUT_DIR" "$CACHE_DIR/build" "$CACHE_DIR/mod"

# --user keeps go.sum and the built .so owned by the invoking user rather than
# root, which matters because the same tree is committed and deployed.
docker run --rm \
	--platform "linux/$TARGETARCH" \
	--user "$(id -u):$(id -g)" \
	-v "$REPO_DIR/go":/src \
	-v "$OUT_DIR":/out \
	-v "$CACHE_DIR":/gocache \
	-w /src \
	-e CGO_ENABLED=1 \
	-e GOOS=linux \
	-e GOARCH="$TARGETARCH" \
	-e GOCACHE=/gocache/build \
	-e GOMODCACHE=/gocache/mod \
	-e GOFLAGS=-buildvcs=false \
	"$GO_IMAGE" \
	sh -c 'go mod tidy && go build -buildmode=c-shared -o /out/codex-turn-state.so .'

echo
echo "built: $OUT_DIR/codex-turn-state.so"
ls -la "$OUT_DIR"
