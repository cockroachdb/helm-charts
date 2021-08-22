#!/bin/bash

tag=$(yq r ./cockroachdb/values.yaml 'tls.selfSigner.image.tag')
echo "Your current tag is ${tag}"
currentCommit=$(git rev-parse HEAD)
lastCommit=$(git rev-parse @~)

git diff "${lastCommit}" "${currentCommit}" cockroachdb/values.yaml | grep "$tag" | grep +
if [[ $? -ne 0 ]]; then
  echo "You have not changed the tag of selfSigner utility"
  exit 1
fi
exit 0
