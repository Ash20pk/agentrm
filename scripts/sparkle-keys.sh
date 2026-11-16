#!/usr/bin/env bash
# Creates the Sparkle update-signing key once (kept in the login keychain) and prints the public
# key to put in app/project.yml as SPARKLE_PUBLIC_KEY. Running it again prints the existing key.
set -euo pipefail
cd "$(dirname "$0")/../app"
BIN=$(find build -type d -path "*artifacts/sparkle/Sparkle/bin" 2>/dev/null | head -1)
[ -n "$BIN" ] || { echo "Build the app once so Xcode fetches Sparkle, then run this again." >&2; exit 1; }
"$BIN/generate_keys" >/dev/null 2>&1 || true
"$BIN/generate_keys" -p
