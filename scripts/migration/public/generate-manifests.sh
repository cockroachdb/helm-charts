#!/usr/bin/env bash

set -euo pipefail
set -x

sts_yaml=$(kubectl get sts -o yaml $CRDBCLUSTER)

echo "${sts_yaml}" | yq "$(cat crdbcluster-template.json)" >manifests/crdbcluster-${CRDBCLUSTER}.yaml

num_nodes=$(echo "${sts_yaml}" | yq '.spec.replicas')

export join_str=""
for idx in $(seq 0 $(($num_nodes - 1))); do
  if [[ -n "${join_str}" ]]; then
    join_str="${join_str},"
  fi
  join_str="${join_str}${CRDBCLUSTER}-${idx}.${CRDBCLUSTER}.${NAMESPACE}:26258"
done

for idx in $(seq 0 $(($num_nodes - 1))); do
  export crdb_node_name=${CRDBCLUSTER}-${idx}
  export k8s_node_name=$(kubectl get pod -o yaml ${crdb_node_name} | yq '.spec.nodeName')
  echo "${sts_yaml}" | yq "$(cat crdbnode-template.json)" >manifests/crdbnode-${CRDBCLUSTER}-${idx}.yaml
done
