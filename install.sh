#!/bin/sh
# Install Remote For Xirp.
#
#   curl -fsSL https://raw.githubusercontent.com/AboveColin/remote-for-xirp/main/install.sh | sh
#
# Downloads the latest release binary to ~/.local/bin and tells you what to run next.
# It deliberately does not install the background service or generate a key — that is
# one explicit command afterwards, so nothing starts listening on your network because
# you piped a script into a shell.
set -eu

REPO="AboveColin/remote-for-xirp"
BIN_DIR="${XIRP_REMOTE_BIN_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
  Darwin) ;;
  *)
    echo "This only runs on macOS: it reads the Xirp app's process environment and drives tmux." >&2
    exit 1
    ;;
esac

echo "Finding the latest release of ${REPO}..."
TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
if [ -z "${TAG:-}" ]; then
  echo "Could not determine the latest release. Check https://github.com/${REPO}/releases" >&2
  exit 1
fi

URL="https://github.com/${REPO}/releases/download/${TAG}/xirp-remote_${TAG}_darwin_universal.tar.gz"
TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

echo "Downloading ${TAG}..."
curl -fsSL "${URL}" -o "${TMP}/x.tar.gz"

# Verify against the published checksums rather than trusting the download.
if curl -fsSL "https://github.com/${REPO}/releases/download/${TAG}/SHA256SUMS" -o "${TMP}/SHA256SUMS"; then
  EXPECTED=$(grep "darwin_universal" "${TMP}/SHA256SUMS" | awk '{print $1}')
  ACTUAL=$(shasum -a 256 "${TMP}/x.tar.gz" | awk '{print $1}')
  if [ -n "${EXPECTED}" ] && [ "${EXPECTED}" != "${ACTUAL}" ]; then
    echo "Checksum mismatch. Expected ${EXPECTED}, got ${ACTUAL}. Not installing." >&2
    exit 1
  fi
  echo "Checksum verified."
fi

tar xzf "${TMP}/x.tar.gz" -C "${TMP}"
mkdir -p "${BIN_DIR}"
mv "${TMP}/xirp-remote" "${BIN_DIR}/xirp-remote"
chmod +x "${BIN_DIR}/xirp-remote"

echo
echo "Installed $("${BIN_DIR}/xirp-remote" version) to ${BIN_DIR}/xirp-remote"
case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *) echo "Note: ${BIN_DIR} is not on your PATH." ;;
esac
echo
echo "Next, with the Xirp app running:"
echo
echo "  xirp-remote interfaces                  # which address to serve on"
echo "  xirp-remote install --generate-key      # start it and print a QR to scan"
echo
