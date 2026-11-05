#!/usr/bin/env bash
# Builds a ReKapture release: the installer, the portable zip and SHA256SUMS.txt.
#
#   scripts/build-release.sh 2.5.0 /path/to/ffmpeg-win64-folder
#
# The FFmpeg folder must contain bin/ffmpeg.exe and LICENSE.txt (a BtbN
# "ffmpeg-master-latest-win64-lgpl" build). Needs Go, go-winres, makensis
# (NSIS 3) and zip. Output goes to dist/<version>/.
#
# Publish it with the GitHub CLI:
#   gh release create v2.5.0 dist/2.5.0/* --title "ReKapture 2.5.0" --notes-file notes.md
set -euo pipefail

VERSION="${1:?usage: build-release.sh <version> <ffmpeg folder>}"
FFMPEG_DIR="${2:?usage: build-release.sh <version> <ffmpeg folder>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if ! grep -q "const appVersion = \"$VERSION\"" app_windows.go; then
  echo "app_windows.go says a different version. Set appVersion = \"$VERSION\" first." >&2
  exit 1
fi
for f in "$FFMPEG_DIR/bin/ffmpeg.exe" "$FFMPEG_DIR/LICENSE.txt"; do
  [ -f "$f" ] || { echo "missing $f" >&2; exit 1; }
done

OUT="dist/$VERSION"
APP="build/app"
rm -rf "$OUT" "$APP"
mkdir -p "$OUT" "$APP/ffmpeg"

echo "== version info and icon"
grep -q "\"$VERSION.0\"" winres/winres.json || { echo "winres/winres.json says a different version. Set it to $VERSION.0 / $VERSION first." >&2; exit 1; }
go-winres make --arch amd64 --in winres/winres.json --out rsrc

echo "== ReKapture.exe"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o "$APP/ReKapture.exe" .
sed 's/$/\r/' README.txt > "$APP/README.txt"
cp "$FFMPEG_DIR/bin/ffmpeg.exe" "$FFMPEG_DIR/LICENSE.txt" "$APP/ffmpeg/"

echo "== installer"
(cd installer && makensis ${MAKENSIS_FLAGS:--V2 -WX} -DVERSION="$VERSION" -DSRC="../$APP" -DOUTFILE="../$OUT/ReKapture-Setup-$VERSION.exe" ReKapture.nsi)

echo "== portable zip"
STAGE="build/portable/ReKapture Portable"
rm -rf build/portable && mkdir -p "$STAGE"
cp -r "$APP/." "$STAGE/"
printf 'This file turns on portable mode.\r\n\r\nWhile it is here, ReKapture keeps its settings and log in the "Data" folder\r\nnext to ReKapture.exe, so you can move this whole folder to another PC or a\r\nUSB drive. Delete this file to use your AppData folder instead.\r\n' > "$STAGE/portable.txt"
(cd build/portable && zip -qr -9 "$ROOT/$OUT/ReKapture-Portable-$VERSION.zip" "ReKapture Portable")

echo "== checksums"
(cd "$OUT" && sha256sum "ReKapture-Setup-$VERSION.exe" "ReKapture-Portable-$VERSION.zip" > SHA256SUMS.txt && cat SHA256SUMS.txt)
ls -la "$OUT"
