#!/usr/bin/env bash
# Prints the Sensu asset definition (for sensuctl create -f) built from a sha512 checksums file.
# Usage: asset-yml.sh <asset name> <download base URL> <checksums file>
set -euo pipefail
name=$1 base_url=$2 checksums=$3

cat <<EOF
---
type: Asset
api_version: core/v2
metadata:
  name: $name
spec:
  builds:
EOF

while read -r sha file; do
  arch=${file##*_linux_}
  arch=${arch%.tar.gz}
  cat <<EOF
    - url: $base_url/$file
      sha512: $sha
      filters:
        - entity.system.os == 'linux'
        - entity.system.arch == '$arch'
EOF
done < "$checksums"
