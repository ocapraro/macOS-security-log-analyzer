#!/usr/bin/env bash
set -euo pipefail

DWELL_SECONDS="${DWELL_SECONDS:-12}"
TEST_DIR="${TEST_DIR:-/tmp/security-analyzer-test}"
LABEL="com.ocapraro.securityanalyzer.test"
LAUNCH_AGENT="$HOME/Library/LaunchAgents/$LABEL.plist"
PAYLOAD_LOG="$TEST_DIR/payload.log"
AGENT_LOG="$TEST_DIR/agent.log"

cleanup() {
  launchctl bootout "gui/$(id -u)" "$LAUNCH_AGENT" >/dev/null 2>&1 || true
  rm -f "$LAUNCH_AGENT"
  rm -rf "$TEST_DIR"
}

trap cleanup EXIT

mkdir -p "$TEST_DIR" "$HOME/Library/LaunchAgents"

curl -L https://example.com -o "$TEST_DIR/downloaded-example.html" >/dev/null 2>&1 || true

cat > "$TEST_DIR/payload.sh" <<'SCRIPT'
#!/bin/sh
echo "harmless test payload ran at $(date)" >> "__PAYLOAD_LOG__"
SCRIPT
sed -i '' "s|__PAYLOAD_LOG__|$PAYLOAD_LOG|g" "$TEST_DIR/payload.sh"
chmod +x "$TEST_DIR/payload.sh"
"$TEST_DIR/payload.sh"

cat > "$TEST_DIR/agent.sh" <<'SCRIPT'
#!/bin/sh
echo "harmless launch agent test at $(date)" >> "__AGENT_LOG__"
SCRIPT
sed -i '' "s|__AGENT_LOG__|$AGENT_LOG|g" "$TEST_DIR/agent.sh"
chmod +x "$TEST_DIR/agent.sh"

cat > "$LAUNCH_AGENT" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$TEST_DIR/agent.sh</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
</dict>
</plist>
PLIST

launchctl bootout "gui/$(id -u)" "$LAUNCH_AGENT" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$(id -u)" "$LAUNCH_AGENT"

sleep "$DWELL_SECONDS"
"$TEST_DIR/payload.sh"

echo "Generated harmless dangerous-looking traffic:"
echo "- downloaded file: $TEST_DIR/downloaded-example.html"
echo "- temp executable: $TEST_DIR/payload.sh"
echo "- temp launch agent target: $TEST_DIR/agent.sh"
echo "- LaunchAgent plist: $LAUNCH_AGENT"
echo
echo "Cleanup completed."
