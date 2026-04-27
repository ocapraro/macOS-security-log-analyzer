#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MONITOR_DIR="$ROOT_DIR/monitor"
OUT_DIR="$MONITOR_DIR/out"
APP_NAME="${APP_NAME:-SecurityAnalyzer}"
BUNDLE_ID="${BUNDLE_ID:-com.example.security-analyzer}"
IDENTITY="${IDENTITY:-}"
PROFILE="${PROFILE:-}"
APP_DIR="$OUT_DIR/$APP_NAME.app"
MACOS_DIR="$APP_DIR/Contents/MacOS"

if [[ -z "$IDENTITY" ]]; then
  echo "error: set IDENTITY to a local Apple signing identity" >&2
  echo "example: IDENTITY='Apple Development: Your Name (TEAMID)' $0" >&2
  exit 1
fi

mkdir -p "$MACOS_DIR"

SDK="$(xcrun --sdk macosx --show-sdk-path)"

clang \
  -fblocks \
  -DENABLE_ENDPOINT_SECURITY \
  -isysroot "$SDK" \
  "$MONITOR_DIR/src/main.c" \
  -lEndpointSecurity \
  -lbsm \
  -o "$MACOS_DIR/$APP_NAME"

cat > "$APP_DIR/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>$APP_NAME</string>
    <key>CFBundleIdentifier</key>
    <string>$BUNDLE_ID</string>
    <key>CFBundleName</key>
    <string>$APP_NAME</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleVersion</key>
    <string>1</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0</string>
    <key>LSBackgroundOnly</key>
    <true/>
</dict>
</plist>
PLIST

if [[ -n "$PROFILE" ]]; then
  cp "$PROFILE" "$APP_DIR/Contents/embedded.provisionprofile"
fi

codesign \
  --force \
  --options runtime \
  --sign "$IDENTITY" \
  --entitlements "$MONITOR_DIR/entitlements.plist" \
  "$APP_DIR"

codesign -d --entitlements :- "$APP_DIR" 2>/dev/null

echo
echo "Built: $APP_DIR"
echo "Run: sudo '$APP_DIR/Contents/MacOS/$APP_NAME'"
