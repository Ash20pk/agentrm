#!/usr/bin/env bash
# Writes packaging/homebrew/agentrm.rb for a release: scripts/update-cask.sh VERSION DMG_SHA256
# Copy the result into the tap repository (Ash20pk/homebrew-tap, Casks/agentrm.rb).
set -euo pipefail
VERSION=$1 SHA=$2
OUT="$(dirname "$0")/../packaging/homebrew/agentrm.rb"
sed -e "s/__VERSION__/$VERSION/g" -e "s/__SHA256__/$SHA/g" "$(dirname "$0")/../packaging/homebrew/agentrm.rb.in" > "$OUT"
echo "wrote $OUT"
