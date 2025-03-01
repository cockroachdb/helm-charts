#!/usr/bin/env bash

# registries.sh manages local Docker registries for development and testing.
# It provides commands to start and stop registry containers using docker-compose.

set -euo pipefail

# Determine the absolute path of the script
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Default configuration
DEFAULT_NETWORK_NAME="k3d-local"

usage() {
    cat << EOF
Usage: $(basename "$0") <command> [network_name]
Commands:
  up   - Start registry containers
  down - Stop registry containers
Network name defaults to ${DEFAULT_NETWORK_NAME}
EOF
    exit 1
}

# Validate input
if [ $# -lt 1 ]; then
    usage
fi

COMMAND="$1"
DOCKER_REGISTRY_PROJECT_NAME="${2:-$DEFAULT_NETWORK_NAME}"

# The name of the docker network. This must change if it changes in the docker-compose.yaml file.
DOCKER_REGISTRY_NETWORK_NAME="${2:-$DEFAULT_NETWORK_NAME}"

case $COMMAND in
    up)
        # Create network if it doesn't exist
        docker network create --driver bridge "${DOCKER_REGISTRY_NETWORK_NAME}" || true
        
        # Start containers
        DOCKER_NETWORK_NAME="${DOCKER_REGISTRY_NETWORK_NAME}" \
            docker-compose -p "${DOCKER_REGISTRY_PROJECT_NAME}" \
            -f "${SCRIPT_DIR}/docker-compose.yaml" up -d
        ;;
    down)
        # Stop containers
        DOCKER_NETWORK_NAME="${DOCKER_REGISTRY_NETWORK_NAME}" \
            docker-compose -p "${DOCKER_REGISTRY_PROJECT_NAME}" \
            -f "${SCRIPT_DIR}/docker-compose.yaml" down
        ;;
    *)
        echo "Error: Unknown command: $COMMAND"
        usage
        ;;
esac
