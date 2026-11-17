#!/bin/sh
# Puts the pinned yt-dlp of a platform (tools/ytdlp.lock) at OUT:
#   tools/ytdlp.sh OS/ARCH OUT
# Downloads it once into $YTDLP_CACHE (default $HOME/.cache/antresol/yt-dlp)
# and checks its sha256 every time. YTDLP_BIN=<file> puts that file instead,
# unchecked (a development build with a stand-in).
set -eu
[ $# = 2 ] || { echo "usage: ytdlp.sh OS/ARCH OUT" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
lock=$here/ytdlp.lock
mkdir -p "$(dirname "$2")"
if [ -n "${YTDLP_BIN:-}" ]; then
	cp "$YTDLP_BIN" "$2" && chmod 755 "$2"
	exit 0
fi
version=$(sed -n 's/^VERSION=//p' "$lock")
line=$(grep "^$1 " "$lock" || true)
[ -n "$version" ] && [ -n "$line" ] || { echo "ytdlp.sh: no pin for $1 in $lock" >&2; exit 1; }
asset=$(echo "$line" | cut -d' ' -f2)
sum=$(echo "$line" | cut -d' ' -f3)
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
cache=${YTDLP_CACHE:-$HOME/.cache/antresol/yt-dlp}/$version
mkdir -p "$cache"
if [ ! -f "$cache/$asset" ] || [ "$(sha "$cache/$asset")" != "$sum" ]; then
	rm -f "$cache/$asset.part"
	curl -fsSL -o "$cache/$asset.part" "https://github.com/yt-dlp/yt-dlp/releases/download/$version/$asset"
	[ "$(sha "$cache/$asset.part")" = "$sum" ] || { rm -f "$cache/$asset.part"; echo "ytdlp.sh: $asset $version: sha256 mismatch" >&2; exit 1; }
	mv "$cache/$asset.part" "$cache/$asset"
fi
cp "$cache/$asset" "$2"
chmod 755 "$2"
