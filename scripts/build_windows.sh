#!/usr/bin/env sh
set -eu

APP_NAME="file-transfer"
ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist/windows"

mkdir -p "$DIST_DIR"

echo "Building Windows executables..."

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "$DIST_DIR/${APP_NAME}-windows-amd64.exe" \
  "$ROOT_DIR"

CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "$DIST_DIR/${APP_NAME}-windows-arm64.exe" \
  "$ROOT_DIR"

echo "Done:"
echo "  $DIST_DIR/${APP_NAME}-windows-amd64.exe"
echo "  $DIST_DIR/${APP_NAME}-windows-arm64.exe"
