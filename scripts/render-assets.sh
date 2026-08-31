#!/usr/bin/env bash
# Render all pi-gchat assets from their SVG sources (requires rsvg-convert).
# Re-run after editing any assets/*.svg file.
set -euo pipefail
cd "$(dirname "$0")/.."

rsvg-convert -w 512 -h 512 assets/avatar.svg           -o assets/avatar.png
rsvg-convert -w 128 -h 128 assets/avatar.svg           -o assets/avatar-128.png
rsvg-convert -w 32  -h 32  assets/avatar-32.svg        -o assets/avatar-32.png
rsvg-convert -w 220 -h 140 assets/banner-220x140.svg   -o assets/banner-220x140.png
rsvg-convert -w 1280 -h 800 assets/screenshot-intro.svg -o assets/screenshot-intro.png

echo "rendered:"
ls -l assets/*.png
