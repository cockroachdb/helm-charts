#!/usr/bin/env bash

set -euxo pipefail

remove_artifacts() {
  rm -rfv ./build/artifacts
}
trap remove_artifacts EXIT

build/make.sh
build/release.sh
