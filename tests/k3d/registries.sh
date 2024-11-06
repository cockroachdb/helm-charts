#!/usr/bin/env bash

set -euxo pipefail

# Figure out, regardless of any symlinks, aliases, etc, where this script
# is located.
SOURCE="${BASH_SOURCE[0]}"
while [ -h "$SOURCE" ] ; do SOURCE="$(readlink "$SOURCE")"; done
DIR="$( cd -P "$( dirname "$SOURCE" )" && pwd )"


COMMAND="${1-}"

DOCKER_REGISTRY_PROJECT_NAME=${2:-k3d-local}

# The name of the docker network. This must change if it changes in the docker-compose.yaml file.
DOCKER_REGISTRY_NETWORK_NAME=${2:-k3d-local}


case $COMMAND in
  up)
    docker network create --driver bridge ${DOCKER_REGISTRY_NETWORK_NAME} || true
    DOCKER_NETWORK_NAME=${DOCKER_REGISTRY_NETWORK_NAME} docker-compose -p ${DOCKER_REGISTRY_PROJECT_NAME} -f ${DIR}/docker-compose.yaml up -d
  ;;
  down)
    DOCKER_NETWORK_NAME=${DOCKER_REGISTRY_NETWORK_NAME} docker-compose -p ${DOCKER_REGISTRY_PROJECT_NAME} -f ${DIR}/docker-compose.yaml down
  ;;
  *)
    echo "Unknown command: $COMMAND"
    exit 1;
  ;;
esac
