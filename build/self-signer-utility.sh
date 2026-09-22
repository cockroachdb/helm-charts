#!/bin/bash

tag=$(bin/yq '.tls.selfSigner.image.tag' ./cockroachdb-legacy/values.yaml)
echo "Your current tag is ${tag}"
currentCommit=$(git rev-parse HEAD)
lastCommit=$(git rev-parse @~)

git diff "${lastCommit}" "${currentCommit}" cockroachdb-legacy/values.yaml | grep -w "$tag" | grep +
if [[ $? -eq 0 ]]; then
  echo "You have changed the tag of selfSigner utility"
fi
