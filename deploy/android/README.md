# Android

## The constraint, first

An Android wrapper cannot be built once and handed to everyone. It is a Trusted Web
Activity: Chrome renders one origin full-screen, and that origin is **baked into the APK
at build time** and verified by fetching `/.well-known/assetlinks.json` from it.

Every instance of this app lives somewhere different and usually private — a LAN address,
or a hostname behind a VPN. There is no single origin to wrap, so there is no APK to
publish. That is why releases contain a binary and not an APK.

What can be built is an APK for **your** origin, which is what the pipeline does.

## Install the PWA instead (no build, and it keeps notifications)

Open the pairing link and use Chrome's "Install app". You get a home-screen icon, a
full-screen app, and — unlike a hand-rolled WebView wrapper — Web Push keeps working,
because it is still Chrome underneath.

For most people this is the answer, and nothing is lost by stopping here.

## Build an APK for your own origin

Two ways, same result.

### In CI

Run the **apk** workflow (Actions > apk > Run workflow) with your origin and an
application id. It needs four repository secrets, which are yours to create — a signing
key produced by CI is a key anyone could sign with:

```sh
keytool -genkey -v -keystore android.keystore -alias upload \
        -keyalg RSA -keysize 2048 -validity 10000
base64 -i android.keystore | pbcopy
```

| Secret | Value |
|---|---|
| `ANDROID_KEYSTORE_BASE64` | the base64 you just copied |
| `ANDROID_KEYSTORE_PASSWORD` | the store password |
| `ANDROID_KEY_PASSWORD` | the key password |
| `ANDROID_KEY_ALIAS` | `upload` |

The workflow refuses to run without them and prints these instructions. It builds inside
the Bubblewrap project's own container image, which already has the JDK and Android
command line tools.

### On your machine

`build-apk.sh` does the same thing locally. Bubblewrap will download the Android SDK on
first run, about 1 GB.

```sh
export XIRP_ORIGIN=https://xirp.example.com
./build-apk.sh
```

## Then: the asset links file

Both routes produce `assetlinks.json` alongside the APK. Copy it to the machine running
the service:

```sh
cp assetlinks.json ~/Library/Application\ Support/xirp-remote/assetlinks.json
```

`xirp-remote` serves it at `/.well-known/assetlinks.json`, unauthenticated, because the
check happens before any user is involved. Without it the app still runs, but with a
browser address bar across the top.

## Why not a plain WebView wrapper

It would accept any URL at runtime and so could ship as one APK for everyone. It would
also lose Web Push, since the Push API is not available to a bare `WebView` — and being
told an agent finished is the main reason to have this on a phone.
