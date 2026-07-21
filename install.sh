#!/bin/sh
set -e

REPO="BenjaminBenetti/fleet-man"
INSTALL_DIR="/usr/local/bin"
BINARY="fleet"
VERSION=""

# Parse flags
while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            VERSION="$2"
            shift 2
            ;;
        *)
            echo "Error: unknown flag: $1"
            exit 1
            ;;
    esac
done

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)
        echo "Error: unsupported architecture: $ARCH"
        exit 1
        ;;
esac

# Detect OS and map it to the release asset's platform token. Releases ship a
# binary per OS/arch pair (fleet-linux-amd64, fleet-darwin-arm64, ...); pick the
# one matching this machine.
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
    linux)         OS="linux" ;;
    darwin)        OS="darwin" ;;
    *)
        echo "Error: unsupported OS: $OS (supported: linux, darwin)"
        exit 1
        ;;
esac

ASSET="fleet-${OS}-${ARCH}"

# Determine version
if [ -n "$VERSION" ]; then
    TAG="$VERSION"
    echo "Using specified version: ${TAG}"
else
    echo "Fetching latest release..."
    TAG=$(curl -sL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | cut -d '"' -f 4)

    if [ -z "$TAG" ]; then
        echo "Error: could not determine latest release"
        exit 1
    fi
fi

URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"

echo "Downloading fleet ${TAG} (${OS}/${ARCH})..."
TMP=$(mktemp)
HTTP_CODE=$(curl -sL -o "$TMP" -w "%{http_code}" "$URL")

if [ "$HTTP_CODE" != "200" ]; then
    rm -f "$TMP"
    echo "Error: download failed (HTTP ${HTTP_CODE})"
    echo "URL: ${URL}"
    exit 1
fi

chmod +x "$TMP"

# Install — use sudo if needed
if [ -w "$INSTALL_DIR" ]; then
    mv "$TMP" "${INSTALL_DIR}/${BINARY}"
else
    echo "Installing to ${INSTALL_DIR} (requires sudo)..."
    sudo mv "$TMP" "${INSTALL_DIR}/${BINARY}"
fi

echo "fleet ${TAG} installed to ${INSTALL_DIR}/${BINARY}"
