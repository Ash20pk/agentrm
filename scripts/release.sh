#!/usr/bin/env bash
# Builds a Developer ID signed, notarized and stapled AgentRM (.zip and .dmg), the Sparkle
# appcast that tells installed copies about it, and the Homebrew cask.
#
# Needs, once per machine:
#   - an Apple ID in Xcode → Settings → Accounts, for a team that can create Developer ID
#     certificates; DEVELOPMENT_TEAM = <team id> in app/Config/Local.xcconfig
#   - the Sparkle signing key in the login keychain (scripts/sparkle-keys.sh creates it once)
# Then: scripts/release.sh, and publish build/release/* as a GitHub release tagged v<version>.
set -euo pipefail
cd "$(dirname "$0")/../app"
VERSION=$(awk -F'"' '/MARKETING_VERSION/{print $2; exit}' project.yml)
OUT="$PWD/build/release"
rm -rf "$OUT" && mkdir -p "$OUT"

xcodegen generate -q
xcodebuild -project AgentRM.xcodeproj -scheme AgentRM -configuration Release \
  -archivePath "$OUT/AgentRM.xcarchive" -derivedDataPath build/dd -allowProvisioningUpdates archive |
  grep -E "error|ARCHIVE (SUCCEEDED|FAILED)"

echo "Signing with Developer ID and uploading for notarization…"
xcodebuild -exportArchive -archivePath "$OUT/AgentRM.xcarchive" -exportOptionsPlist ExportOptions.plist \
  -exportPath "$OUT/upload" -allowProvisioningUpdates

echo "Waiting for Apple's notary service…"
for i in $(seq 1 60); do
  if xcodebuild -exportNotarizedApp -archivePath "$OUT/AgentRM.xcarchive" -exportPath "$OUT/app" >/dev/null 2>&1; then
    break
  fi
  [ "$i" = 60 ] && { echo "Notarization did not finish in 30 minutes." >&2; exit 1; }
  sleep 30
done
APP="$OUT/app/AgentRM.app"
xcrun stapler validate "$APP"
spctl --assess --type execute -v "$APP"
codesign -dv "$APP/Contents/Helpers/agentrm" 2>&1 | grep -q "Developer ID Application" ||
  { echo "the bundled agentrm helper is not Developer ID signed" >&2; exit 1; }

ZIP="$OUT/AgentRM-$VERSION.zip"
ditto -c -k --keepParent "$APP" "$ZIP"
STAGE="$OUT/dmg" && mkdir -p "$STAGE"
cp -R "$APP" "$STAGE/" && ln -s /Applications "$STAGE/Applications"
DMG="$OUT/AgentRM-$VERSION.dmg"
hdiutil create -volname "AgentRM $VERSION" -srcfolder "$STAGE" -ov -format UDZO "$DMG" >/dev/null

# Sparkle: the appcast lists the zip with its EdDSA signature; installed copies only accept an
# update signed with the key whose public half is in their Info.plist.
SPARKLE_BIN=$(find build/dd/SourcePackages/artifacts -type d -path "*Sparkle/bin" | head -1)
[ -n "$SPARKLE_BIN" ] || { echo "Sparkle tools not found; build the app once first" >&2; exit 1; }
FEED="$OUT/feed" && mkdir -p "$FEED" && cp "$ZIP" "$FEED/"
"$SPARKLE_BIN/generate_appcast" --download-url-prefix "https://github.com/Ash20pk/agentrm/releases/download/v$VERSION/" \
  -o "$OUT/appcast.xml" "$FEED"

(cd "$OUT" && shasum -a 256 "$(basename "$ZIP")" "$(basename "$DMG")" | tee SHA256SUMS)
"$(dirname "$0")/../scripts/update-cask.sh" "$VERSION" "$(shasum -a 256 "$DMG" | cut -d' ' -f1)"
echo "Done. Publish: gh release create v$VERSION $ZIP $DMG $OUT/appcast.xml $OUT/SHA256SUMS -R Ash20pk/agentrm"
