#1/usr/bin/env bash

set -euo pipefail

sts_yaml=$(kubectl get sts -o yaml ${release_name}-cockroachdb)

export release_name=$(kubectl get sts -o yaml crdb-test-cockroachdb | yq '.metadata.annotations."meta.helm.sh/release-name"')
export namespace=$(echo "${sts_yaml}" | yq '.metadata.annotations."meta.helm.sh/release-namespace"')

echo "${sts_yaml}" | yq "$(cat crdbcluster-template.json)" >crdbcluster-${release_name}.yaml

num_nodes=$(echo "${sts_yaml}" | yq '.spec.replicas')

# Because the statefulset and cloud operator use different grpc ports,
# configure crdb to use both port numbers for the `--join` flag. Note that this
# flag will be overwritten by the operator by the end of the migration.
export join_str=""
for idx in $(seq 0 $(($num_nodes - 1))); do
  if [[ -n "${join_str}" ]]; then
    join_str="${join_str},"
  fi
  join_str="${join_str}${release_name}-cockroachdb-${idx}.${release_name}-cockroachdb.${namespace}:26257,"
  join_str="${join_str}${release_name}-cockroachdb-${idx}.${release_name}-cockroachdb.${namespace}:26258"
done

for idx in $(seq 0 $(($num_nodes - 1))); do
  export crdb_node_name=${release_name}-cockroachdb-${idx}
  export k8s_node_name=$(kubectl get pod -o yaml ${crdb_node_name} | yq '.spec.nodeName')
  echo "${sts_yaml}" | yq "$(cat crdbnode-template.json)" >crdbnode-${release_name}-${idx}.yaml
done
