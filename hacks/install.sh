#!/usr/bin/env sh

# install.sh downloads and installs the pgclone release binary.

set -e

REPO="warpcomdev/pgclone"
VERSION="0.1.0"
INSTALLATION_PATH="${INSTALLATION_PATH:-/usr/local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)

case "$os" in
    linux)
        os="linux"
        ;;
    *)
        echo "Unsupported OS: $os" >&2
        exit 1
        ;;
esac

case "$arch" in
    x86_64|amd64)
        arch="amd64"
        ;;
    *)
        echo "Unsupported architecture: $arch" >&2
        exit 1
        ;;
esac

asset="pgclone_${os}_${arch}.tar.gz"
url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

checksums_url="https://github.com/${REPO}/releases/download/${VERSION}/pgclone_${VERSION}_checksums.txt"

echo "Downloading ${asset}..."
if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$tmpdir/$asset"
    curl -fsSL "$checksums_url" -o "$tmpdir/checksums.txt"
elif command -v wget >/dev/null 2>&1; then
    wget -q "$url" -O "$tmpdir/$asset"
    wget -q "$checksums_url" -O "$tmpdir/checksums.txt"
else
    echo "curl or wget is required" >&2
    exit 1
fi

echo "Verifying checksum..."
(
    cd "$tmpdir"
    sha256sum -c "checksums.txt" --quiet 2>/dev/null || \
        shasum -a 256 -c "checksums.txt" --quiet 2>/dev/null || \
        { echo "Checksum verification failed" >&2; exit 1; }
)

echo "Extracting..."
tar -xzf "$tmpdir/$asset" -C "$tmpdir"

echo "Installing pgclone to ${INSTALLATION_PATH}..."
mkdir -p "$INSTALLATION_PATH"
install -m 755 "$tmpdir/cmd" "$INSTALLATION_PATH/pgclone"

echo "pgclone v${VERSION} installed to ${INSTALLATION_PATH}/pgclone"
