#!/usr/bin/env bash

set -euo pipefail

image=cockroachdb/charts-builder
version=20200702-210440

function init() {
  docker build --tag="${image}" "$(dirname "${0}")/builder"
}

if [ "${1-}" = "pull" ]; then
  docker pull "${image}:${version}"
  exit 0
fi

if [ "${1-}" = "init" ]; then
  init
  exit 0
fi

if [ "${1-}" = "push" ]; then
  init
  tag=$(date +%Y%m%d-%H%M%S)
  docker tag "${image}" "${image}:${tag}"
  docker push "${image}:${tag}"
  echo "New image: ${image}:${tag}"
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

host_home=${helm_charts_toplevel}/build/builder_home
mkdir -p "${host_home}"

container_home=/charts/

# Run our build container with a set of volumes mounted that will
# allow the container to store persistent build data on the host
# computer.
vols="--volume=${helm_charts_toplevel}:${container_home}"

# -i causes some commands (including `git diff`) to attempt to use
# a pager, so we override $PAGER to disable.

# shellcheck disable=SC2086
docker run --init --privileged -i ${tty-} --rm \
  ${vols} \
  --workdir="${container_home}" \
  --env="PAGER=cat" \
  --env="TZ=America/New_York" \
  "${image}:${version}" "$@"
