#!/usr/bin/env sh

set -eu

version="${1:?Usage: render-install.sh VERSION}"

case "$version" in
    *[!0-9A-Za-z.+-]*)
        echo "Invalid release version: $version" >&2
        exit 1
        ;;
esac

mkdir -p .release
sed "s/__VERSION__/${version}/g" hacks/install.sh > .release/install.sh
chmod 755 .release/install.sh
