#!/usr/bin/env sh
set -eu

APP_NAME="file-transfer"
ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist/linux"

mkdir -p "$DIST_DIR"

echo "Building Linux executables..."

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "$DIST_DIR/${APP_NAME}-linux-amd64" \
  "$ROOT_DIR"

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "$DIST_DIR/${APP_NAME}-linux-arm64" \
  "$ROOT_DIR"

chmod +x "$DIST_DIR/${APP_NAME}-linux-amd64" "$DIST_DIR/${APP_NAME}-linux-arm64"

echo "Done:"
echo "  $DIST_DIR/${APP_NAME}-linux-amd64"
echo "  $DIST_DIR/${APP_NAME}-linux-arm64"
