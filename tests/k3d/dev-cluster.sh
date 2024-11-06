#!/usr/bin/env bash

CLUSTER_NAME=local

NETWORK_NAME=k3d-local

if [ $# -eq 0 ]
  then
      echo "No arguments supplied: "
      echo "  up: Start cluster."
      echo "     --nodes x: The cluster should have x nodes (default 1)"
      echo "     --version x: The version of Kubernetes (default 1.24.14)"
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

case $COMMAND in
  up)
    k3d cluster create ${CLUSTER_NAME} \
      --network ${NETWORK_NAME} \
			--registry-config "$SCRIPT_DIR/registries.yaml" \
			--image rancher/k3s:v${version}-k3s1 \
			--agents ${nodes} \
      --k3s-node-label "topology.kubernetes.io/region=us-east-1@agent:0" \
      --k3s-node-label "topology.kubernetes.io/region=us-east-1@server:0" 
  ;;
  down)
    k3d cluster delete ${CLUSTER_NAME}
  ;;
  *)
    echo "Unknown command: $COMMAND"
    exit 1;
  ;;
esac
