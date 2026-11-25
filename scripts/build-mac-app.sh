#!/bin/sh
# Builds the MediaLayer Mac player (proof of concept) into
# apps/macos/MediaLayer.app. Needs Go and the Xcode Command Line Tools;
# full Xcode is not needed.
#
#   scripts/build-mac-app.sh          # release build
#   scripts/build-mac-app.sh debug    # debug build
#
# The app signs in to Google Drive with MediaLayer's own OAuth client,
# read from ~/Library/Application Support/MediaLayer/google-oauth.json:
#   {"client_id": "...apps.googleusercontent.com", "client_secret": "..."}
set -eu

config=${1:-release}
root=$(cd "$(dirname "$0")/.." && pwd)
app="$root/apps/macos"

echo "Building the Go core..."
mkdir -p "$app/build"
(cd "$root" && CGO_ENABLED=1 GOOS=darwin GOARCH=$(go env GOHOSTARCH) \
  MACOSX_DEPLOYMENT_TARGET=14.0 CGO_CFLAGS="-O2 -g -mmacosx-version-min=14.0" \
  go build -buildmode=c-archive -o "$app/build/libmlcore.a" ./cmd/mlcore)
cp "$app/build/libmlcore.h" "$app/Sources/CMLCore/include/libmlcore.h"

echo "Building the app..."
(cd "$app" && swift build -c "$config" -Xlinker -L"$app/build")
bin=$(cd "$app" && swift build -c "$config" --show-bin-path)/MediaLayer

bundle="$app/MediaLayer.app"
rm -rf "$bundle"
mkdir -p "$bundle/Contents/MacOS"
cp "$bin" "$bundle/Contents/MacOS/MediaLayer"
cat > "$bundle/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>MediaLayer</string>
  <key>CFBundleDisplayName</key><string>MediaLayer</string>
  <key>CFBundleIdentifier</key><string>org.medialayer.MediaLayer</string>
  <key>CFBundleExecutable</key><string>MediaLayer</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>LSMinimumSystemVersion</key><string>14.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSPrincipalClass</key><string>NSApplication</string>
  <key>NSAppTransportSecurity</key>
  <dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict>
</plist>
PLIST
codesign --force --sign - "$bundle"
echo "Built $bundle"
