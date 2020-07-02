#!/usr/bin/env bash

set -euo pipefail

image="quay.io/helmpack/chart-testing"
version="v2.4.1"

if [ "${1-}" = "pull" ]; then
  docker pull "${image}:${version}"
  exit 0
fi

if [ "${1-}" = "version" ]; then
  echo "${version}"
  exit 0
fi

if [ -t 0 ]; then
  tty=--tty
fi

# Absolute path to the toplevel helm-charts directory.
helm_charts_toplevel=$(dirname "$(cd "$(dirname "${0}")"; pwd)")

container_home=/charts/

# Run our tester container with a set of volumes mounted that will
# allow the container to store persistent data on the host computer.

vols="--volume=${helm_charts_toplevel}:${container_home}"

# -i causes some commands (including `git diff`) to attempt to use
# a pager, so we override $PAGER to disable.

# shellcheck disable=SC2086
docker run --init --privileged -i ${tty-} --rm \
  ${vols} \
  --workdir="/charts" \
  --env="PAGER=cat" \
  --env="TZ=America/New_York" \
  "${image}:${version}" "$@"
