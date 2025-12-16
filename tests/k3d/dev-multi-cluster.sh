#!/usr/bin/env bash

# dev-multi-cluster.sh manages multiple k3d clusters for development and testing.
# It supports creating and deleting clusters with configurable nodes, versions, and regions.

set -euo pipefail

# Default configuration
DEFAULT_NODES=1
DEFAULT_CLUSTERS=1
DEFAULT_K8S_VERSION="1.30.6"
DEFAULT_CLUSTER_NAME="local"

# Region and networking configuration
REGIONS=("us-east1" "us-east2" "us-east3" "us-east4" "us-east5" "us-east6")
AVAILABILITY_ZONES=(a b c d e f g h i j k l m n o p q r s t u v w x y z)
#SUBNETS=(192.168.1.0/24 192.168.2.0/24 192.168.3.0/24 192.168.4.0/24 192.168.5.0/24 192.168.6.0/24)
SERVICE_CIDRS=(10.0.128.0/17 10.1.128.0/17 10.2.128.0/17)
CLUSTER_CIDRS=(10.0.0.0/17 10.1.0.0/17 10.2.0.0/17)

# Binary paths
K3D_PATH="./bin/k3d"

# Registry configuration
REGISTRY="gcr.io"
REPOSITORY="cockroachlabs-helm-charts/cockroach-self-signer-cert"

# Required container images for the clusters.
# These images are imported into each cluster during creation.
# Add any additional images here.
REQUIRED_IMAGES=(
    "quay.io/jetstack/cert-manager-cainjector:v1.11.0"
    "quay.io/jetstack/cert-manager-webhook:v1.11.0"
    "quay.io/jetstack/cert-manager-controller:v1.11.0"
    "quay.io/jetstack/cert-manager-ctl:v1.11.0"
    "coredns/coredns:1.9.2"
    "$(bin/yq '.cockroachdb.crdbCluster.image.name' ./cockroachdb-parent/charts/cockroachdb/values.yaml)"
    "us-docker.pkg.dev/releases-prod/self-hosted/inotifywait@sha256:2e443a2d00e6541bd596219204865db74f813e8d54678ce2fe71747e40254182"
    "bash:latest"
    "busybox"
    "us-docker.pkg.dev/releases-prod/self-hosted/init-container@sha256:bcfc9312af84c7966f017c2325981b30314c0c293491f942e54da1667bedaf69"
    "${REGISTRY}/${REPOSITORY}:$(bin/yq '.cockroachdb.tls.selfSigner.image.tag' ./cockroachdb-parent/charts/cockroachdb/values.yaml)"
)

usage() {
    cat << EOF
Usage: $(basename "$0") <command> [options]

Commands:
    up   - Start clusters
    down - Delete clusters

Options:
    --nodes <n>        Number of nodes per cluster (default: ${DEFAULT_NODES})
    --clusters <n>     Number of clusters to create (default: ${DEFAULT_CLUSTERS})
    --version <ver>    Kubernetes version (default: ${DEFAULT_K8S_VERSION})
    --name <name>      Base name for clusters (default: ${DEFAULT_CLUSTER_NAME})
    --network_name <n> Network name (default: k3d-\${name})
    --zones <n>        Number of zones for node labels (default: 3)
EOF
    exit 1
}

# Validate input.
if [ $# -eq 0 ]; then
    usage
fi

COMMAND="${1}"
shift

# Parse command line arguments.
while [ $# -gt 0 ]; do
    if [[ $1 == *"--"* ]]; then
        param="${1/--/}"
        declare "$param"="$2"
        shift 2
    else
        shift
    fi
done

# Set defaults if not provided.
nodes=${nodes:-$DEFAULT_NODES}
clusters=${clusters:-$DEFAULT_CLUSTERS}
version=${version:-$DEFAULT_K8S_VERSION}
name=${name:-$DEFAULT_CLUSTER_NAME}
network_name=${network_name:-"k3d-${name}"}
zones=${zones:-3}

# Function to create K3D clusters
create_clusters() {
  for ((i=0; i<clusters; i++)); do
      region="${REGIONS[$i]}"
      cluster_name="${name}-cluster-${i}"

      # Check if the cluster exists
      if "${K3D_PATH}" cluster list --output name | grep -q "${cluster_name}"; then
        echo "Cluster '${cluster_name}' already exists. Skipping creation."
        clusters=$((clusters + 1))
        continue
      else
        echo "Cluster '${cluster_name}' does not exist. Creating it..."
      fi

      # Get network configuration for this cluster
      svc_cidr=${SERVICE_CIDRS[$i]}
      cluster_cidr=${CLUSTER_CIDRS[$i]}

    # K3d cluster configuration
    local k3d_args=(
        "${K3D_PATH}" cluster create "${cluster_name}"
#       --subnet ${subnet}
        --no-lb
#       --servers-memory 2GB
       --k3s-arg "--service-cidr=${svc_cidr}@server:*"
       --k3s-arg "--cluster-cidr=${cluster_cidr}@server:*"
        --k3s-arg "--disable=traefik@server:*"
        --k3s-arg "--disable-network-policy@server:*"
#       --k3s-arg "--disable=servicelb@server:0"
#       --k3s-arg "--disable=coredns@server:0"
        --k3s-arg "--flannel-backend=none@server:*"
        --k3s-arg="--tls-san=host.k3d.internal@server:*"
#       --k3s-arg "--disable=kube-proxy@server:0"
        --runtime-ulimit "nofile=65535:1048576"
#       --agents-memory 2GB
        --network "${network_name}"                      # Use the same network name for pulling images from local registries
#       --registry-config "$SCRIPT_DIR/registries.yaml"  # Use this flag if k3s containers need access to local registries
        --agents ${nodes} # Number of agent nodes        # Use this Add more worker nodes
    )

    # Only add --image if version is explicitly set by user
    # else use the latest default k8s version
    if [[ "${version}" != "${DEFAULT_K8S_VERSION}" ]]; then
        k3d_args+=(--image "rancher/k3s:${version}-k3s1")
    fi

    if ! "${k3d_args[@]}"; then
        echo "Error creating K3D cluster: ${cluster_name}"
        return 1
    fi

     configure_node_labels "${cluster_name}" "${i}" "${region}"
     import_container_images "${cluster_name}"

     echo "Cluster '${cluster_name}' created and configured successfully."
  done
}

# Label the nodes with region and zone topology labels.
configure_node_labels() {
    local cluster_name="$1"
    local cluster_index="$2"
    local region="$3"

    # Label server node with region.
    server_node=$(kubectl --context "k3d-${cluster_name}" get nodes -l node-role.kubernetes.io/control-plane=true -o jsonpath='{.items[0].metadata.name}')
    kubectl --context "k3d-${cluster_name}" label node "$server_node" "topology.kubernetes.io/region=${region}"

    # Label server node with a default zone, e.g. using the first zone from the list.
    # We want the server node also to be a schedulable node for cockroachdb pod.
    server_zone="${region}${AVAILABILITY_ZONES[0]}"
    kubectl --context "k3d-${cluster_name}" label node "$server_node" "topology.kubernetes.io/zone=${server_zone}"

    # Remove the zone labeled for server node.
    available_agent_zones=("${AVAILABILITY_ZONES[@]:1}")

    # Label agent nodes with region and zone.
    agent_nodes=$(kubectl --context "k3d-${cluster_name}" get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}')

    agent_index=0
    for node in $agent_nodes; do
        # Calculate a unique zone for each agent.
        zone_suffix="${available_agent_zones[$(( (cluster_index * zones) + agent_index )) % ${#available_agent_zones[@]}]}"
        zone="${region}${zone_suffix}"

        kubectl --context "k3d-${cluster_name}" label node "$node" \
            "topology.kubernetes.io/region=${region}" \
            "topology.kubernetes.io/zone=${zone}"

        agent_index=$((agent_index + 1))
    done
}

# Pull images that don't exist locally and import them into the k3d cluster.
import_container_images() {
    local cluster_name="$1"
    echo "Pulling and importing required container images..."
    for image in "${REQUIRED_IMAGES[@]}"; do
        if ! docker image inspect "$image" >/dev/null 2>&1; then
            echo "Pulling image: $image"
            docker pull "$image"
        fi
    done
    "${K3D_PATH}" image import "${REQUIRED_IMAGES[@]}" -c "${cluster_name}"
}

# Function to delete K3D clusters.
delete_clusters() {
      echo "Deleting all K3d clusters..."
      ${K3D_PATH} cluster delete --all
      echo "K3d clusters deleted successfully."
}


# Main case to run based on the command.
case $COMMAND in
    up)
        create_clusters
        if [[ $? -ne 0 ]]; then
            echo "Cluster creation failed."
            exit 1
        fi
        echo "All clusters created successfully."
        ;;
    down)
        delete_clusters
        ;;
    *)
        echo "Unknown command: $COMMAND"
        usage
        ;;
esac