#!/usr/bin/env bash
# Builds a Developer ID signed, notarized and stapled AgentRM (.zip and .dmg), the Sparkle
# appcast that tells installed copies about it, and the Homebrew cask.
#
# Needs, once per machine:
#   - an Apple ID in Xcode → Settings → Accounts, for a team that can create Developer ID
#     certificates; DEVELOPMENT_TEAM = <team id> in app/Config/Local.xcconfig
#   - the Sparkle signing key in the login keychain (scripts/sparkle-keys.sh creates it once)
#   - optionally NOTARY_PROFILE=<name> (a `xcrun notarytool store-credentials` profile), so a
#     rejected submission's log can be fetched and printed
# Then: bump MARKETING_VERSION in app/project.yml, commit, tag v<version>, run
# scripts/release.sh from a clean tree, and publish build/release/* as the GitHub release v<version>.
#
# Build number. Sparkle offers an update only when the appcast's sparkle:version (the
# CFBundleVersion of the new build) is higher than the installed one. It is derived from
# MARKETING_VERSION as major*10000 + minor*100 + patch (0.1.0 → 100, 0.2.0 → 200, 1.0.3 →
# 10003), so it rises with every release and is above the "1" the first builds shipped with.
# Minor and patch must stay below 100. The script refuses to build a number not above the
# newest one in the published appcast.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT/app"
die() { echo "release: $*" >&2; exit 1; }

VERSION=$(awk -F'"' '/MARKETING_VERSION/{print $2; exit}' project.yml)
[[ $VERSION =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] || die "MARKETING_VERSION '$VERSION' is not major.minor.patch"
MAJOR=$((10#${BASH_REMATCH[1]})) MINOR=$((10#${BASH_REMATCH[2]})) PATCH=$((10#${BASH_REMATCH[3]}))
((MINOR < 100 && PATCH < 100)) || die "minor and patch must be below 100 for the build number ($VERSION)"
BUILD=$((MAJOR * 10000 + MINOR * 100 + PATCH))

# What is built is exactly what is tagged.
[ -z "$(git status --porcelain --untracked-files=normal)" ] ||
  { git status --short >&2; die "the working tree is not clean; commit or stash first"; }
TAGS=$(git tag --points-at HEAD)
grep -qx "v$VERSION" <<<"$TAGS" ||
  die "HEAD is not tagged v$VERSION (MARKETING_VERSION). Tag it: git tag v$VERSION && git push origin v$VERSION"

# The new build number must be above everything installed copies may already have.
FEED_URL=$(awk '/SUFeedURL:/{print $2; exit}' project.yml)
CURRENT=$(mktemp)
trap 'rm -f "$CURRENT"' EXIT
CODE=$(curl -sSL -o "$CURRENT" -w '%{http_code}' "$FEED_URL") || die "could not fetch $FEED_URL"
case "$CODE" in
  # The element (Sparkle 2) or the older enclosure attribute.
  200) NEWEST=$({ grep -oE 'sparkle:version(>|=")[0-9]+' "$CURRENT" || true; } | grep -oE '[0-9]+$' | sort -n | tail -1) ;;
  404) NEWEST=0; echo "No published appcast yet: first release." ;;
  *) die "fetching $FEED_URL answered HTTP $CODE" ;;
esac
NEWEST=${NEWEST:-0}
((BUILD > NEWEST)) ||
  die "build number $BUILD (from $VERSION) is not above $NEWEST, the newest in the published appcast: Sparkle would offer no update"
echo "Releasing $VERSION, build $BUILD (published appcast: $NEWEST)."

OUT="$PWD/build/release"
rm -rf "$OUT" && mkdir -p "$OUT"

xcodegen generate -q
xcodebuild -project AgentRM.xcodeproj -scheme AgentRM -configuration Release \
  -archivePath "$OUT/AgentRM.xcarchive" -derivedDataPath build/dd -allowProvisioningUpdates \
  CURRENT_PROJECT_VERSION="$BUILD" archive >"$OUT/archive.log" 2>&1 ||
  { grep -E "error|ARCHIVE FAILED" "$OUT/archive.log" >&2 || tail -40 "$OUT/archive.log" >&2; die "archive failed (full log: $OUT/archive.log)"; }
grep -E "ARCHIVE SUCCEEDED" "$OUT/archive.log"
BUILT=$(/usr/libexec/PlistBuddy -c "Print :CFBundleVersion" "$OUT/AgentRM.xcarchive/Products/Applications/AgentRM.app/Contents/Info.plist")
[ "$BUILT" = "$BUILD" ] || die "the archive's CFBundleVersion is '$BUILT', not $BUILD"

echo "Signing with Developer ID and uploading for notarization…"
xcodebuild -exportArchive -archivePath "$OUT/AgentRM.xcarchive" -exportOptionsPlist ExportOptions.plist \
  -exportPath "$OUT/upload" -allowProvisioningUpdates 2>&1 | tee "$OUT/upload.log"

# Prints what Apple's notary service said about the submission, then fails.
notary_failed() {
  echo "--- xcodebuild -exportNotarizedApp said:" >&2
  cat "$OUT/notary.log" >&2
  local id
  id=$(grep -hoiE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' "$OUT/notary.log" "$OUT/upload.log" | tail -1 || true)
  if [ -n "$id" ] && [ -n "${NOTARY_PROFILE:-}" ]; then
    echo "--- notary log for submission $id:" >&2
    xcrun notarytool log "$id" --keychain-profile "$NOTARY_PROFILE" >&2 || true
  elif [ -n "$id" ]; then
    echo "Fetch the notary log with: xcrun notarytool log $id --keychain-profile <profile>" >&2
  fi
  die "$1"
}

echo "Waiting for Apple's notary service…"
TRIES=${NOTARY_TRIES:-60}
for i in $(seq 1 "$TRIES"); do
  if xcodebuild -exportNotarizedApp -archivePath "$OUT/AgentRM.xcarchive" -exportPath "$OUT/app" >"$OUT/notary.log" 2>&1; then
    break
  fi
  # A rejection is final: say so now, not after half an hour of "waiting".
  if grep -qiE 'invalid|rejected|not notarized|notarization failed|failed notarization' "$OUT/notary.log"; then
    notary_failed "Apple rejected the submission"
  fi
  [ "$i" = "$TRIES" ] && notary_failed "notarization did not finish in $((TRIES / 2)) minutes; run again later: xcodebuild -exportNotarizedApp -archivePath $OUT/AgentRM.xcarchive -exportPath $OUT/app"
  if grep -qiE 'in progress|pending|not (yet )?(complete|finished|available|ready)' "$OUT/notary.log"; then
    echo "  still in progress ($i/$TRIES)"
  else
    # Not a message we know: show it rather than wait in silence.
    echo "  not ready ($i/$TRIES); xcodebuild said:" && sed 's/^/    /' "$OUT/notary.log" | tail -15
  fi
  sleep 30
done
APP="$OUT/app/AgentRM.app"
xcrun stapler validate "$APP"
spctl --assess --type execute -v "$APP"
# Captured first: piping into grep -q under pipefail fails when grep exits early (SIGPIPE).
HELPER_SIG=$(codesign -dv --verbose=2 "$APP/Contents/Helpers/agentrm" 2>&1) || true
grep -q "Authority=Developer ID Application" <<<"$HELPER_SIG" ||
  die "the bundled agentrm helper is not Developer ID signed"

ZIP="$OUT/AgentRM-$VERSION.zip"
ditto -c -k --keepParent "$APP" "$ZIP"
STAGE="$OUT/dmg" && mkdir -p "$STAGE"
cp -R "$APP" "$STAGE/" && ln -s /Applications "$STAGE/Applications"
DMG="$OUT/AgentRM-$VERSION.dmg"
hdiutil create -volname "AgentRM $VERSION" -srcfolder "$STAGE" -ov -format UDZO "$DMG" >/dev/null

# Sparkle: the appcast lists the zip with its EdDSA signature; installed copies only accept an
# update signed with the key whose public half is in their Info.plist.
SPARKLE_BIN=$(find build/dd/SourcePackages/artifacts -type d -path "*Sparkle/bin" | head -1)
[ -n "$SPARKLE_BIN" ] || die "Sparkle tools not found; build the app once first"
FEED="$OUT/feed" && mkdir -p "$FEED" && cp "$ZIP" "$FEED/"
"$SPARKLE_BIN/generate_appcast" --download-url-prefix "https://github.com/Ash20pk/agentrm/releases/download/v$VERSION/" \
  -o "$OUT/appcast.xml" "$FEED"
grep -q "<sparkle:version>$BUILD</sparkle:version>" "$OUT/appcast.xml" ||
  die "the new appcast does not list build $BUILD"

(cd "$OUT" && shasum -a 256 "$(basename "$ZIP")" "$(basename "$DMG")" | tee SHA256SUMS)
"$ROOT/scripts/update-cask.sh" "$VERSION" "$(shasum -a 256 "$DMG" | cut -d' ' -f1)"
echo "Done. Publish: gh release create v$VERSION $ZIP $DMG $OUT/appcast.xml $OUT/SHA256SUMS -R Ash20pk/agentrm"
