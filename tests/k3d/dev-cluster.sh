#!/usr/bin/env bash

# dev-cluster.sh manages k3d clusters for development and testing.
# It supports creating and deleting clusters with configurable nodes, versions, and regions.

set -euo pipefail

# Default configuration
DEFAULT_REGION="us-east-1"
DEFAULT_ZONES=3
DEFAULT_NODES=1
DEFAULT_K8S_VERSION="v1.32.9"
DEFAULT_CLUSTER_NAME="local"

# Binary paths
K3D_PATH="./bin/k3d"

# Registry configuration
REGISTRY="gcr.io"
REPOSITORY="cockroachlabs-helm-charts/cockroach-self-signer-cert"

# Required container images for the cluster.
# These images are imported into the cluster during creation.
REQUIRED_IMAGES=(
    "quay.io/jetstack/cert-manager-cainjector:v1.11.0"
    "quay.io/jetstack/cert-manager-webhook:v1.11.0"
    "quay.io/jetstack/cert-manager-controller:v1.11.0"
    "quay.io/jetstack/cert-manager-ctl:v1.11.0"
    "quay.io/jetstack/trust-manager:v0.17.1"
    "quay.io/jetstack/trust-pkg-debian-bookworm:20230311.0"
    "$(bin/yq '.cockroachdb.crdbCluster.image.name' ./cockroachdb-parent/charts/cockroachdb/values.yaml)"
    "bash:latest"
    "${REGISTRY}/${REPOSITORY}:$(bin/yq '.cockroachdb.tls.selfSigner.image.tag' ./cockroachdb-parent/charts/cockroachdb/values.yaml)"
    "$(bin/yq '.image.registry' ./cockroachdb-parent/charts/operator/values.yaml)/$(bin/yq '.image.repository' ./cockroachdb-parent/charts/operator/values.yaml):$(bin/yq '.image.tag' ./cockroachdb-parent/charts/operator/values.yaml)"
)

usage() {
    cat << EOF
Usage: $(basename "$0") <command> [options]

Commands:
    up   - Start cluster
    down - Delete cluster

Options:
    --nodes <n>        Number of nodes (default: ${DEFAULT_NODES})
    --version <ver>    Kubernetes version (default: ${DEFAULT_K8S_VERSION})
    --name <name>      Cluster name (default: ${DEFAULT_CLUSTER_NAME})
    --network_name <n> Network name (default: k3d-\${name})
    --region <r>       Region for node labels (default: ${DEFAULT_REGION})
    --zones <n>        Number of zones (default: ${DEFAULT_ZONES})
EOF
    exit 1
}

# Validate input
if [ $# -eq 0 ]; then
    usage
fi

COMMAND="${1}"
shift

# Parse command line arguments
while [ $# -gt 0 ]; do
    if [[ $1 == *"--"* ]]; then
        param="${1/--/}"
        declare "$param"="$2"
        shift 2
    else
        shift
    fi
done

# Set defaults if not provided
name=${name:-$DEFAULT_CLUSTER_NAME}
network_name=${network_name:-"k3d-${name}"}
nodes=${nodes:-$DEFAULT_NODES}
version=${version:-$DEFAULT_K8S_VERSION}
region=${region:-$DEFAULT_REGION}
zones=${zones:-$DEFAULT_ZONES}

create_cluster() {
    local cluster_name="${name}-cluster"

    # Check if cluster already exists
    if "${K3D_PATH}" cluster list --output name | grep -q "${cluster_name}"; then
        echo "Cluster '${cluster_name}' already exists. Skipping creation."
        return 0
    fi

    echo "Creating cluster '${cluster_name}'..."

    # K3d cluster configuration
    local k3d_args=(
        "${K3D_PATH}" cluster create "${cluster_name}"
#       --subnet ${subnet}
        --no-lb
#       --servers-memory 2GB
#       --k3s-arg "--service-cidr=${svc}@server:0"
#       --k3s-arg "--cluster-cidr=${cluster}@server:0"
        --k3s-arg "--disable=traefik@server:0"
#        --k3s-arg "--disable-network-policy@server:0"
#       --k3s-arg "--disable=servicelb@server:0"
#       --k3s-arg "--disable=coredns@server:0"
#        --k3s-arg "--flannel-backend=none@server:0"
        --k3s-arg="--tls-san=host.k3d.internal@server:*"
#       --k3s-arg "--disable=kube-proxy@server:0"
        --runtime-ulimit "nofile=65535:1048576"
#       --agents-memory 2GB
        --network "${network_name}"                      # Use the same network name for pulling images from local registries
#       --registry-config "$SCRIPT_DIR/registries.yaml"  # Use this flag if k3s containers need access to local registries
        --agents ${nodes} # Number of agent nodes        # Use this Add more worker nodes
    )

    if ! "${k3d_args[@]}"; then
        echo "Error creating K3D cluster: ${cluster_name}"
        return 1
    fi

    configure_node_labels "${cluster_name}"
    import_container_images "${cluster_name}"

    echo "Cluster '${cluster_name}' created and configured successfully"
}

# Label the nodes with region and zone topology labels.
configure_node_labels() {
    local cluster_name="$1"
    local zones_suffix=(a b c d e f g h i j k l m n o p q r s t u v w x y z)
    
    # Get all nodes in the cluster
    local nodes
    nodes=$(kubectl --context "k3d-${cluster_name}" get nodes -o jsonpath='{.items[*].metadata.name}')

    local index=0
    for node in $nodes; do
        local zone="${region}${zones_suffix[$((index % zones))]}"
        kubectl --context "k3d-${cluster_name}" label node "$node" \
            "topology.kubernetes.io/region=${region}" \
            "topology.kubernetes.io/zone=${zone}"
        index=$((index + 1))
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

case $COMMAND in
    up)
        echo "Creating cluster..."
        if ! create_cluster; then
            echo "Cluster creation failed"
            exit 1
        fi
        
        # Wait for cluster to be ready
        echo "Waiting for cluster to be ready..."
        max_retries=30
        retry_count=0
        while ! kubectl --context "k3d-${name}-cluster" get nodes &>/dev/null; do
            if [ $retry_count -ge $max_retries ]; then
                echo "Timed out waiting for cluster to be ready"
                exit 1
            fi
            sleep 2
            ((retry_count++))
        done
        echo "Cluster is ready"
        ;;
    down)
        "${K3D_PATH}" cluster delete "${name}-cluster"
        ;;
    *)
        echo "Error: Unknown command: $COMMAND"
        usage
        ;;
esac