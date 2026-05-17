#!/usr/bin/env bash
# Wrap the just-built binary in a .app bundle and copy Qt's frameworks
# alongside it via macdeployqt, so the resulting bundle runs on machines
# without Qt installed.
#
# Invoked from .goreleaser.yml as a post-build hook with the absolute path
# to the freshly-built binary, which lives at:
#   dist/diesel_darwin_<arch>*/diesel.app/Contents/MacOS/diesel
set -euo pipefail

BIN="${1:?usage: macos-bundle.sh <path-to-binary>}"
APP="$(cd "$(dirname "$BIN")/../.." && pwd)"   # walk MacOS -> Contents -> .app

mkdir -p "$APP/Contents/Resources"
chmod +x "$BIN"

# Minimal Info.plist. Update CFBundleShortVersionString from a build-time
# variable if you start cutting versioned releases. NSMicrophoneUsageDescription
# is required by macOS for any mic access — without it the OS denies the
# request silently instead of prompting the user.
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleExecutable</key><string>diesel</string>
  <key>CFBundleIdentifier</key><string>co.pettingzoo.diesel</string>
  <key>CFBundleName</key><string>diesel</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1.0</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSMicrophoneUsageDescription</key><string>Diesel records voice messages from your microphone and transcribes them through the speech-to-text endpoint you configure.</string>
</dict></plist>
PLIST

# macdeployqt copies Qt's frameworks into Contents/Frameworks and rewrites
# the main binary's load paths from /opt/homebrew/... to @executable_path/...
#
# `-no-plugins`: Homebrew installs optional Qt plugins (qtvirtualkeyboard,
# qtsvg, image-format plugins for webp/brotli, ...) whose dylibs reference
# their siblings via @rpath that resolves to /opt/homebrew/lib at runtime via
# rpath baked into the parent framework. macdeployqt doesn't inherit that
# rpath, so it fails to resolve those deps and corrupts the bundle's
# ad-hoc signature. We don't use any of those plugins, so we skip plugin
# deployment and only ship the cocoa platform plugin (required for any
# QApplication on macOS) by hand below.
macdeployqt "$APP" -verbose=1 -no-plugins

# Deploy only the platform plugin we actually need.
QT_PLUGINS="$(qmake -query QT_INSTALL_PLUGINS)"
install -d "$APP/Contents/PlugIns/platforms"
cp "$QT_PLUGINS/platforms/libqcocoa.dylib" "$APP/Contents/PlugIns/platforms/"

# Point Qt at the in-bundle plugin dir. macdeployqt normally writes this
# itself, but `-no-plugins` skips it.
cat > "$APP/Contents/Resources/qt.conf" <<'CONF'
[Paths]
Plugins = PlugIns
CONF

# Re-sign the bundle ad-hoc so Gatekeeper accepts the manually-added plugin.
codesign --force --deep --sign - "$APP" >/dev/null 2>&1 || true

echo "Bundled $APP"