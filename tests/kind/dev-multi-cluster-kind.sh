#!/usr/bin/env bash

# dev-multi-cluster-kind.sh manages multiple Kind clusters for development and testing.
# It supports creating and deleting clusters with configurable nodes, versions, and regions.

set -euo pipefail

# Default configuration
DEFAULT_NODES=3
DEFAULT_CLUSTERS=3
DEFAULT_K8S_VERSION="v1.30.6"
DEFAULT_CLUSTER_NAME="local"

# Region and networking configuration
REGIONS=("us-east1" "us-east2" "us-east3" "us-east4" "us-east5" "us-east6")
AVAILABILITY_ZONES=(a b c d e f g h i j k l m n o p q r s t u v w x y z)
SERVICE_CIDRS=(10.0.128.0/17 10.1.128.0/17 10.2.128.0/17)
CLUSTER_CIDRS=(10.0.0.0/17 10.1.0.0/17 10.2.0.0/17)

# Binary paths
KIND_PATH="./bin/kind"

# Registry configuration
REGISTRY="gcr.io"
REPOSITORY="cockroachlabs-helm-charts/cockroach-self-signer-cert"

# Required container images for the clusters.
# These images are imported into each cluster during creation.
REQUIRED_IMAGES=(
    "quay.io/jetstack/cert-manager-cainjector:v1.11.0"
    "quay.io/jetstack/cert-manager-webhook:v1.11.0"
    "quay.io/jetstack/cert-manager-controller:v1.11.0"
    "quay.io/jetstack/cert-manager-ctl:v1.11.0"
    "coredns/coredns:1.9.2"
    "$(bin/yq '.cockroachdb.crdbCluster.image.name' ./cockroachdb-parent/charts/cockroachdb/values.yaml)"
    "us-docker.pkg.dev/cockroach-cloud-images/data-plane/inotifywait:87edf086db32734c7fa083a62d1055d664900840"
    "bash:latest"
    "busybox"
    "us-docker.pkg.dev/cockroach-cloud-images/data-plane/init-container:f21cb0727676a48d0000bc3f32108ce59d51c3e7"
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
    --zones <n>        Number of zones for node labels (default: 3)
    --disable-cni      Disable default CNI (for Calico installation)
    --network_name <n> Network name (default: kind-\${name})
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
        if [[ $1 == *"="* ]]; then
            # Handle --param=value format
            param="${1%%=*}"
            param="${param/--/}"
            param="${param//-/_}"  # Convert hyphens to underscores for valid variable names
            value="${1#*=}"
            declare "$param"="$value"
            shift
        else
            # Handle --param value format
            param="${1/--/}"
            param="${param//-/_}"  # Convert hyphens to underscores for valid variable names
            declare "$param"="$2"
            shift 2
        fi
    else
        shift
    fi
done

# Set defaults if not provided
nodes=${nodes:-$DEFAULT_NODES}
clusters=${clusters:-$DEFAULT_CLUSTERS}
version=${version:-$DEFAULT_K8S_VERSION}
name=${name:-$DEFAULT_CLUSTER_NAME}
zones=${zones:-3}
disable_cni=${disable_cni:-false}
network_name=${network_name:-"kind-${name}"}

# Function to create Kind clusters
create_clusters() {
  for ((i=0; i<clusters; i++)); do
      region="${REGIONS[$i]}"
      cluster_name="${name}-cluster-${i}"

      # Check if the cluster exists
      if "${KIND_PATH}" get clusters | grep -q "${cluster_name}"; then
        echo "Cluster '${cluster_name}' already exists. Skipping creation."
        continue
      else
        echo "Cluster '${cluster_name}' does not exist. Creating it..."
      fi

      # Get network configuration for this cluster
      svc_cidr=${SERVICE_CIDRS[$i]}
      cluster_cidr=${CLUSTER_CIDRS[$i]}

      # Create Kind cluster configuration
      cat > "/tmp/kind-config-${i}.yaml" << EOF
apiVersion: kind.x-k8s.io/v1alpha4
kind: Cluster
name: ${cluster_name}
networking:
  serviceSubnet: "${svc_cidr}"
  podSubnet: "${cluster_cidr}"
EOF

      # Add CNI configuration if disabled
      if [[ "$disable_cni" == "true" ]]; then
        cat >> "/tmp/kind-config-${i}.yaml" << EOF
  disableDefaultCNI: true
EOF
      fi

      # Use a single shared Docker network for all clusters to enable cross-cluster L2 reachability
      cluster_network="${network_name}"

      # Add containerdConfigPatches for proper runtime configuration
      cat >> "/tmp/kind-config-${i}.yaml" << EOF
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry.mirrors."localhost:${REGISTRY}"]
    endpoint = ["http://${network_name}-registry:5000"]
EOF

      # Add nodes configuration with unique API server port (start from 7443 to avoid conflicts)
      api_server_port=$((7443 + i))
      cat >> "/tmp/kind-config-${i}.yaml" << EOF
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 6443
    hostPort: ${api_server_port}
    protocol: TCP
EOF

      # Add worker nodes
      for ((n=1; n<=nodes; n++)); do
        echo "- role: worker" >> "/tmp/kind-config-${i}.yaml"
      done

      # Create the cluster
      if ! "${KIND_PATH}" create cluster --config "/tmp/kind-config-${i}.yaml" --image "kindest/node:${version}"; then
        echo "Error creating Kind cluster: ${cluster_name}"
        return 1
      fi

      # Ensure and connect to shared network for consistent MetalLB IP detection (single or multi cluster)
      if ! docker network inspect "${cluster_network}" >/dev/null 2>&1; then
        echo "Creating shared network: ${cluster_network}"
        docker network create "${cluster_network}"
      fi
      # Connect all nodes to the shared network
      for node in $(docker ps --filter "label=io.x-k8s.kind.cluster=${cluster_name}" --format "{{.Names}}"); do
        echo "Connecting node ${node} to network ${cluster_network}"
        docker network connect "${cluster_network}" "${node}" || true
      done

      # Fix kubeconfig to use the correct API server port first
      fix_kubeconfig_server_url "${cluster_name}" "${api_server_port}"

      # Configure node labels for the cluster
      configure_node_labels "${cluster_name}" "${i}" "${region}"

      # Remove NoSchedule taint from control-plane to make it schedulable
      remove_control_plane_taint "${cluster_name}"

      # Import container images
      import_container_images "${cluster_name}"

      echo "Cluster '${cluster_name}' created and configured successfully."

      # Clean up temporary config file
      rm -f "/tmp/kind-config-${i}.yaml"
  done
}

# Label the nodes with region and zone topology labels.
configure_node_labels() {
    local cluster_name="$1"
    local cluster_index="$2"
    local region="$3"

    # Wait for nodes to be ready and label control-plane node with region.
    echo "Waiting for control-plane node to be ready..."
    for i in {1..60}; do
        # First check if kubectl can connect to the cluster
        if ! kubectl --context "kind-${cluster_name}" cluster-info >/dev/null 2>&1; then
            echo "Waiting for cluster API to be ready... (attempt $i/60)"
            sleep 3
            continue
        fi

        # Then check for control-plane node
        server_node=$(kubectl --context "kind-${cluster_name}" get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
        if [[ -n "$server_node" ]]; then
            echo "Found control-plane node: $server_node"
            break
        fi
        echo "Waiting for control-plane node... (attempt $i/60)"
        sleep 3
    done

    if [[ -z "$server_node" ]]; then
        echo "Error: Could not find control-plane node for cluster ${cluster_name}"
        return 1
    fi

    kubectl --context "kind-${cluster_name}" label node "$server_node" "topology.kubernetes.io/region=${region}"

    # Label control-plane node with a default zone, e.g. using the first zone from the list.
    # We want the control-plane node also to be a schedulable node for cockroachdb pod.
    server_zone="${region}${AVAILABILITY_ZONES[0]}"
    kubectl --context "kind-${cluster_name}" label node "$server_node" "topology.kubernetes.io/zone=${server_zone}"

    # Remove the zone labeled for control-plane node.
    available_worker_zones=("${AVAILABILITY_ZONES[@]:1}")

    # Label worker nodes with region and zone.
    worker_nodes=$(kubectl --context "kind-${cluster_name}" get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

    worker_index=0
    for node in $worker_nodes; do
        # Calculate a unique zone for each worker.
        zone_suffix="${available_worker_zones[$(( (cluster_index * zones) + worker_index )) % ${#available_worker_zones[@]}]}"
        zone="${region}${zone_suffix}"

        kubectl --context "kind-${cluster_name}" label node "$node" \
            "topology.kubernetes.io/region=${region}" \
            "topology.kubernetes.io/zone=${zone}"
        worker_index=$((worker_index + 1))
    done
}

# Remove NoSchedule taint from control-plane nodes to make them schedulable
remove_control_plane_taint() {
    local cluster_name="$1"

    echo "Removing NoSchedule taint from control-plane nodes in cluster: ${cluster_name}"

    # Get control-plane nodes
    control_plane_nodes=$(kubectl --context "kind-${cluster_name}" get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[*].metadata.name}')

    # Remove the NoSchedule taint from each control-plane node
    for node in $control_plane_nodes; do
        echo "Removing taint from node: ${node}"
        kubectl --context "kind-${cluster_name}" taint nodes "${node}" node-role.kubernetes.io/control-plane:NoSchedule- || true
    done

    echo "Control-plane nodes are now schedulable"
}

# Pull images that don't exist locally and import them into the Kind cluster.
import_container_images() {
    local cluster_name="$1"
    echo "Pulling and importing required container images..."

    for image in "${REQUIRED_IMAGES[@]}"; do
        # Skip empty image names
        if [[ -z "$image" ]]; then
            continue
        fi

        echo "Processing image: $image"

        # Pull image if it doesn't exist locally
        if ! docker image inspect "$image" >/dev/null 2>&1; then
            echo "Pulling image: $image"
            if ! docker pull "$image"; then
                echo "Warning: Failed to pull image $image, skipping..."
                continue
            fi
        fi

        # Load image into Kind cluster one by one to avoid bulk loading issues
        echo "Loading image $image into cluster $cluster_name..."
        if ! "${KIND_PATH}" load docker-image "$image" --name "${cluster_name}"; then
            echo "Warning: Failed to load image $image into cluster $cluster_name"
        fi
    done
    
    echo "Finished importing container images"
}

# Fix kubeconfig to use the correct API server port for the cluster
fix_kubeconfig_server_url() {
    local cluster_name="$1"
    local api_port="$2"
    local context_name="kind-${cluster_name}"
    
    echo "Updating kubeconfig server URL for cluster ${cluster_name} to use port ${api_port}"
    
    # Update the server URL to use the correct port
    kubectl config set-cluster "${context_name}" --server="https://127.0.0.1:${api_port}"
    
    # Wait for the API server to be ready with retries
    echo "Waiting for API server to be ready on port ${api_port}..."
    for i in {1..30}; do
        if kubectl --context "${context_name}" cluster-info >/dev/null 2>&1; then
            echo "Successfully connected to cluster ${cluster_name} on port ${api_port}"
            return 0
        fi
        echo "Waiting for API server... (attempt $i/30)"
        sleep 2
    done
    
    echo "Warning: Could not connect to cluster ${cluster_name} on port ${api_port}"
    return 1
}

# Function to delete Kind clusters.
delete_clusters() {
    echo "Deleting all Kind clusters..."
    "${KIND_PATH}" delete clusters --all
    echo "Kind clusters deleted successfully."
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
