#!/usr/bin/env bash

set -euxo pipefail

remove_artifacts() {
  rm -rfv ./build/artifacts
}
trap remove_artifacts EXIT

# Build and publish the legacy statefulset chart.
build/make.sh
build/release.sh

# Build and publish v2 charts (operator + cockroachdb from cockroachdb-parent).
build/make.sh v2
build/release.sh v2
