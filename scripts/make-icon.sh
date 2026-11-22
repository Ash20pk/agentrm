#!/bin/sh
# Renders the app icon set and the README logo from app/Resources/AppIcon.svg.
# Needs rsvg-convert (brew install librsvg). Usage: scripts/make-icon.sh
set -eu
cd "$(dirname "$0")/.."
svg=app/Resources/AppIcon.svg
set=app/Resources/Assets.xcassets/AppIcon.appiconset
command -v rsvg-convert >/dev/null || { echo "rsvg-convert not found: brew install librsvg" >&2; exit 1; }
for s in 16 32 128 256 512; do
    rsvg-convert -w "$s" -h "$s" "$svg" -o "$set/icon_${s}x${s}.png"
    rsvg-convert -w $((s * 2)) -h $((s * 2)) "$svg" -o "$set/icon_${s}x${s}@2x.png"
done
rsvg-convert -w 256 -h 256 "$svg" -o docs/logo.png
echo "icon set and docs/logo.png rendered from $svg"
