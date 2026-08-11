#!/usr/bin/env bash
# Wrap the PWA as an Android APK (a Trusted Web Activity).
#
# A TWA is Chrome rendering your site full-screen with no browser UI, shipped as an
# installable app. It is the same code as the PWA, so there is no second client to
# keep in sync.
#
# This is NOT run as part of any build here, because it needs things this repo will
# not decide for you:
#
#   1. The Android SDK (bubblewrap downloads it on first run, ~1 GB).
#   2. A signing keystore, which is yours to create and keep — an APK signed with a
#      key from a git repo is an APK anyone can impersonate.
#   3. An HTTPS origin with a valid certificate. A TWA verifies the site over TLS and
#      falls back to showing a browser address bar if verification fails.
#
# Steps:
#
#   export XIRP_ORIGIN=https://xirp.example.com     # your HTTPS origin
#   keytool -genkey -v -keystore android.keystore \
#           -alias xirp -keyalg RSA -keysize 2048 -validity 10000
#   ./build-apk.sh
#
# Then, for the app to launch without an address bar, publish the asset link the
# script prints to:  $XIRP_ORIGIN/.well-known/assetlinks.json
set -euo pipefail

: "${XIRP_ORIGIN:?set XIRP_ORIGIN to your https origin, e.g. https://xirp.example.com}"
KEYSTORE="${KEYSTORE:-android.keystore}"
ALIAS="${ALIAS:-xirp}"

if ! command -v java >/dev/null; then
  echo "java is required (bubblewrap needs a JDK)" >&2
  exit 1
fi
if [ ! -f "$KEYSTORE" ]; then
  echo "No keystore at $KEYSTORE. Create one first:" >&2
  echo "  keytool -genkey -v -keystore $KEYSTORE -alias $ALIAS -keyalg RSA -keysize 2048 -validity 10000" >&2
  exit 1
fi

# npx fetches bubblewrap on demand; no global install to go stale.
npx --yes @bubblewrap/cli init \
  --manifest "$XIRP_ORIGIN/manifest.json" \
  --directory ./twa

cd ./twa
npx --yes @bubblewrap/cli build --skipPwaValidation

echo
echo "APK:  $(pwd)/app-release-signed.apk"
echo
echo "Install it with:  adb install -r app-release-signed.apk"
echo "Or copy it to the phone and open it (allow installing from this source)."
echo
echo "To remove the address bar, serve this at $XIRP_ORIGIN/.well-known/assetlinks.json :"
npx --yes @bubblewrap/cli fingerprint generateAssetLinks --keystore "../$KEYSTORE" --keyName "$ALIAS" || true
