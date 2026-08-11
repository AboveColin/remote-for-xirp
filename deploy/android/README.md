# Android

Two ways to get this on a phone, in the order most people want them.

## 1. Install the PWA (no build needed)

Open the pairing link — `xirp-remote qr` prints one, or scan the code in Settings —
then use Chrome's "Install app" / "Add to home screen". It launches full-screen with
its own icon, and the service worker means opening it out of range shows the app shell
rather than Chrome's offline page.

This is the same code as the APK below, so nothing is lost by stopping here.

## 2. Wrap it as an APK

`build-apk.sh` produces a Trusted Web Activity: Chrome rendering the site full-screen,
packaged as an installable APK. One codebase, no second client.

It is not wired into any build here because it needs three things this repo should not
decide for you:

- **The Android SDK.** bubblewrap downloads it on first run, about 1 GB.
- **A signing keystore.** Yours to create and keep. A key committed to a repository is
  a key anyone can sign with.
- **An HTTPS origin with a valid certificate.** A TWA verifies the origin; if
  verification fails it still runs but shows a browser address bar.

```sh
export XIRP_ORIGIN=https://xirp.example.com
keytool -genkey -v -keystore android.keystore -alias xirp \
        -keyalg RSA -keysize 2048 -validity 10000
./build-apk.sh
```

To drop the address bar, publish the asset-link JSON the script prints at
`$XIRP_ORIGIN/.well-known/assetlinks.json`.
