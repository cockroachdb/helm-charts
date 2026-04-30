#!/usr/bin/env bash

set -euxo pipefail

remove_artifacts() {
  rm -rfv ./build/artifacts
}
trap remove_artifacts EXIT

# Build and publish the legacy statefulset chart.
build/make.sh
build/release.sh

# Build and publish v2 charts only when explicitly enabled. This keeps the
# first v2 publish under manual control while preserving the legacy release path.
if [ "${PUBLISH_V2:-false}" = "true" ]; then
  build/make.sh v2
  build/release.sh v2
else
  echo "Skipping v2 chart publish because PUBLISH_V2 is not true."
fi
