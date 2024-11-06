#!/usr/bin/env bash
region="us-east-1"
zones=3

if [ $# -eq 0 ]
  then
      echo "No arguments supplied: "
      echo "  up: Start cluster."
      echo "     --nodes x: The cluster should have x nodes (default 1)"
      echo "     --version x: The version of Kubernetes (default 1.24.14)"
      echo "     --name x: The name of the cluster (default local)"
      echo "     --network_name x: The name of the cluster's network (default k3d-\${name})"
      echo "     --region x: The name of the cluster's region for node labels topology.kubernetes.io/region (default us-east-1)"
      echo "     --zones x: The number of zones in the region for node labels topology.kubernetes.io/zone (default 3)"
      
      echo "  down: Delete cluster."

      exit 1
fi

COMMAND="${1-}"
SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )

nodes=${environment:-1}
version=${version:-1.24.14}

while [ $# -gt 0 ]; do

   if [[ $1 == *"--"* ]]; then
        param="${1/--/}"
        declare $param="$2"
        # echo $1 $2 // Optional to see the parameter:value result
   fi

  shift
done

name=${name:-local}
network_name=${network_name:-"k3d-${name}"}

# Function to set topology.kubernetes.io/zone labels in a round-robin fashion
set_node_labels() {
  local nodes=$1
  local region=$2
  local zones=$3
  local labels=""
  local az=(a b c d e f g h i j k l m n o p q r s t u v w x y z)

  for ((i=0; i<nodes; i++)); do
    zone="${region}${az[$((i % zones))]}"
    labels+="--k3s-node-label topology.kubernetes.io/zone=${zone}@agent:${i} "
    labels+="--k3s-node-label topology.kubernetes.io/region=${region}@agent:${i} "
  done

  echo "${labels}"
}

case $COMMAND in
  up)
    node_labels=$(set_node_labels ${nodes} ${region} ${zones})
    k3d cluster create ${name} \
      --network ${network_name} \
			--registry-config "$SCRIPT_DIR/registries.yaml" \
			--image rancher/k3s:v${version}-k3s1 \
			--agents ${nodes} \
      --k3s-node-label "topology.kubernetes.io/region=${region}@server:0" \
      ${node_labels} 
  ;;
  down)
    k3d cluster delete ${name}
  ;;
  *)
    echo "Unknown command: $COMMAND"
    exit 1;
  ;;
esac
