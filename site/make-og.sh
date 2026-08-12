#!/bin/sh
# Regenerates the social card from its source SVG.
#
# sips is part of macOS and rasterises SVG at the document's own width and height, so
# 1200x630 comes out of the markup rather than a flag. No dependency to install, which is
# the whole reason the card is an SVG and not a hand-drawn PNG.
set -eu
cd "$(dirname "$0")"
sips -s format png og.svg --out og.png >/dev/null
sips -g pixelWidth -g pixelHeight og.png
