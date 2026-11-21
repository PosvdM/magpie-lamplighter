#!/bin/sh
# Builds the Lamplighter program for every platform the npm package ships,
# statically (CGO_ENABLED=0), into dist/bin/<goos>-<goarch>/, and writes the
# version to dist/bin/VERSION for scripts/check-dist.mjs.
# Usage: sh scripts/build.sh [version]
# The version defaults to package.json's.
set -eu
cd "$(dirname "$0")/.."
VERSION=${1:-$(node -p "require('./package.json').version")}
rm -rf dist/bin
for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  GOOS=${platform%/*}
  GOARCH=${platform#*/}
  EXT=
  [ "$GOOS" = windows ] && EXT=.exe
  echo "building $GOOS/$GOARCH"
  CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -buildvcs=false -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "dist/bin/$GOOS-$GOARCH/lamplighter$EXT" .
done
printf '%s\n' "$VERSION" > dist/bin/VERSION
