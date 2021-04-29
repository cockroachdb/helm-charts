#!/usr/bin/env bash

set -euo pipefail

image="crdb-chart-tester"
version="latest"


helm_charts_toplevel=$(dirname "$(cd "$(dirname "${0}")"; pwd)")


docker build --tag=${image} -f ./build/tester/Dockerfile ${helm_charts_toplevel}


# Absolute path to the toplevel helm-charts directory.


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
