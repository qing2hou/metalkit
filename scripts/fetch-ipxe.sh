#!/bin/bash
# Downloads iPXE binaries from boot.ipxe.org into internal/ipxebin/assets/
# These are embedded into the controller binary at compile time via go:embed.
set -euo pipefail

DEST="$(cd "$(dirname "$0")/.." && pwd)/internal/ipxebin/assets"
BASE_URL="https://boot.ipxe.org"

mkdir -p "$DEST"
# undionly.kpxe (BIOS) lives in root; snponly.efi/ipxe.efi moved to x86_64-efi/
declare -A FILES=(
    ["undionly.kpxe"]="undionly.kpxe"
    ["snponly.efi"]="x86_64-efi/snponly.efi"
    ["ipxe.efi"]="x86_64-efi/ipxe.efi"
)
for name in "${!FILES[@]}"; do
    src="${FILES[$name]}"
    echo "fetching $name from $src..."
    curl -fsSL -o "$DEST/$name.tmp" "$BASE_URL/$src"
    mv "$DEST/$name.tmp" "$DEST/$name"
done

echo
echo "=== iPXE binaries fetched to $DEST ==="
ls -la "$DEST"
echo
echo "SHA-256 (pin these in this script if you want strict verification):"
sha256sum "$DEST"/*.kpxe "$DEST"/*.efi
echo
echo "If you want strict verification, edit fetch-ipxe.sh and add the expected sha256 values + comparison."
