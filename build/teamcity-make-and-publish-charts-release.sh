#!/usr/bin/env bash

set -euxo pipefail

remove_artifacts() {
  make clean
}
trap remove_artifacts EXIT

build/make.sh
build/release.sh
