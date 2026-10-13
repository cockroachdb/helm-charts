#!/bin/bash

set -euo pipefail

baseRef=${1:-HEAD~1}
tag=$(bin/yq '.tls.selfSigner.image.tag' ./cockroachdb-legacy/values.yaml)
previousTag=$(git show "${baseRef}:cockroachdb-legacy/values.yaml" | bin/yq '.tls.selfSigner.image.tag' -)
echo "Your current tag is ${tag}"

if [[ "$tag" != "$previousTag" ]]; then
  echo "You have changed the tag of selfSigner utility"
fi
