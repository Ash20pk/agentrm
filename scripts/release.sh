#!/usr/bin/env bash
# Builds a Developer ID signed, notarized and stapled AgentRM, as a .zip and a .dmg.
#
# Needs: an Apple ID signed in under Xcode → Settings → Accounts for a team that can create
# Developer ID certificates, and DEVELOPMENT_TEAM = <team id> in app/Config/Local.xcconfig.
# Xcode creates the Developer ID certificate and notarizes through that account.
set -euo pipefail
cd "$(dirname "$0")/../app"
VERSION=$(awk -F'"' '/MARKETING_VERSION/{print $2; exit}' project.yml)
OUT="$PWD/build/release"
rm -rf "$OUT" && mkdir -p "$OUT"

xcodegen generate -q
xcodebuild -project AgentRM.xcodeproj -scheme AgentRM -configuration Release \
  -archivePath "$OUT/AgentRM.xcarchive" -allowProvisioningUpdates archive | grep -E "error|ARCHIVE (SUCCEEDED|FAILED)"

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

(cd "$OUT" && shasum -a 256 "$(basename "$ZIP")" "$(basename "$DMG")" | tee SHA256SUMS)
echo "Done: $ZIP $DMG"
