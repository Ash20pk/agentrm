#!/usr/bin/env bash
# Builds a signed, notarized, stapled AgentRM.dmg.
#
# Needs, once per machine:
#   - a "Developer ID Application" certificate in the login keychain
#   - notarization credentials:  xcrun notarytool store-credentials agentrm-notary \
#         --apple-id <apple id> --team-id <team id>
#   - DEVELOPMENT_TEAM = <team id> in app/Config/Local.xcconfig
set -euo pipefail
cd "$(dirname "$0")/../app"
PROFILE="${NOTARY_PROFILE:-agentrm-notary}"
VERSION=$(awk -F'"' '/MARKETING_VERSION/{print $2; exit}' project.yml)
OUT="$PWD/build/release"
rm -rf "$OUT" && mkdir -p "$OUT"

security find-identity -v -p codesigning | grep -q "Developer ID Application" ||
  { echo "No Developer ID Application certificate in the keychain." >&2; exit 1; }

xcodegen generate -q
xcodebuild -project AgentRM.xcodeproj -scheme AgentRM -configuration Release \
  -derivedDataPath build/dd clean build | grep -E "error|BUILD (SUCCEEDED|FAILED)"
APP=build/dd/Build/Products/Release/AgentRM.app
codesign --verify --deep --strict --verbose=2 "$APP"
codesign -dv "$APP" 2>&1 | grep -q "Developer ID Application" || { echo "app is not Developer ID signed" >&2; exit 1; }

echo "Notarizing the app…"
ditto -c -k --keepParent "$APP" "$OUT/AgentRM.zip"
xcrun notarytool submit "$OUT/AgentRM.zip" --keychain-profile "$PROFILE" --wait
xcrun stapler staple "$APP"

echo "Building the DMG…"
STAGE="$OUT/dmg" && mkdir -p "$STAGE"
cp -R "$APP" "$STAGE/" && ln -s /Applications "$STAGE/Applications"
DMG="$OUT/AgentRM-$VERSION.dmg"
hdiutil create -volname "AgentRM $VERSION" -srcfolder "$STAGE" -ov -format UDZO "$DMG" >/dev/null
IDENTITY=$(security find-identity -v -p codesigning | awk -F'"' '/Developer ID Application/{print $2; exit}')
codesign --sign "$IDENTITY" --timestamp "$DMG"
xcrun notarytool submit "$DMG" --keychain-profile "$PROFILE" --wait
xcrun stapler staple "$DMG"
spctl --assess --type open --context context:primary-signature -v "$DMG"

shasum -a 256 "$DMG" | tee "$DMG.sha256"
echo "Done: $DMG"
