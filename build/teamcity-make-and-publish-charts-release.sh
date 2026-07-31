#!/usr/bin/env bash

set -euxo pipefail

remove_artifacts() {
  rm -rfv ./build/artifacts
}
trap remove_artifacts EXIT

# Build and publish the legacy statefulset chart.
build/make.sh
build/release.sh

# Build and publish the v2 operator and CockroachDB charts.
build/make.sh v2
build/release.sh v2
