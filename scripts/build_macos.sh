#!/usr/bin/env sh
set -eu

APP_NAME="file-transfer"
ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist/macos"

mkdir -p "$DIST_DIR"

echo "Building macOS executables..."

CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "$DIST_DIR/${APP_NAME}-macos-amd64" \
  "$ROOT_DIR"

CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "$DIST_DIR/${APP_NAME}-macos-arm64" \
  "$ROOT_DIR"

if command -v lipo >/dev/null 2>&1; then
  lipo -create \
    "$DIST_DIR/${APP_NAME}-macos-amd64" \
    "$DIST_DIR/${APP_NAME}-macos-arm64" \
    -output "$DIST_DIR/${APP_NAME}-macos-universal"
  chmod +x "$DIST_DIR/${APP_NAME}-macos-universal"
  echo "  $DIST_DIR/${APP_NAME}-macos-universal"
else
  echo "lipo not found; skipped universal macOS binary."
fi

chmod +x "$DIST_DIR/${APP_NAME}-macos-amd64" "$DIST_DIR/${APP_NAME}-macos-arm64"

echo "Done:"
echo "  $DIST_DIR/${APP_NAME}-macos-amd64"
echo "  $DIST_DIR/${APP_NAME}-macos-arm64"
