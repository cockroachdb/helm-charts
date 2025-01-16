#!/bin/bash

Help()
{
   # Display Help
   echo "This script performs Helm upgrade involving new PVCs. Kindly run it from the root of the repository."
   echo
   echo "usage: ./scripts/upgrade_with_new_pvc.sh <release_name> <chart> <chart_version> <values_file> <namespace> <sts_name> <num_replicas> [kubeconfig]"
   echo
   echo "options:"
   echo "release_name: Helm release name, e.g. my-release"
   echo "chart: Helm chart to use (either referenced locally, or to the Helm repository), e.g. cockroachdb/cockroachdb"
   echo "chart_version: Helm chart version to upgrade to, e.g. 15.0.0"
   echo "values_file: Path to the values file, e.g. ./cockroachdb/values.yaml"
   echo "namespace: Kubernetes namespace, e.g. default"
   echo "sts_name: Statefulset name (can be obtained through \"kubectl get sts\"), e.g. my-release-cockroachdb"
   echo "num_replicas: Number of replicas in the statefulset, e.g. 3"
   echo "kubeconfig (optional): Path to the kubeconfig file. Default is $HOME/.kube/config."
   echo
   echo "example: ./scripts/upgrade_with_new_pvc.sh my-release cockroachdb/cockroachdb 15.0.0 ./cockroachdb/values.yaml default my-release-cockroachdb 3"
   echo
}

while getopts ":h" option; do
   case $option in
      h) # display Help
         Help
         exit;;
     \?) # incorrect option
         echo "Error: Invalid option"
         exit;;
   esac
done

release_name=$1
chart=$2
chart_version=$3
values_file=$4
namespace=$5
sts_name=$6
num_replicas=$7
kubeconfig=${8:-$HOME/.kube/config}

# For each replica, do the following:
# 1. Delete the statefulset
# 2. Delete the pod replica
# 3. Upgrade the Helm chart

for i in $(seq 0 $((num_replicas-1))); do
  echo "========== Iteration $((i+1)) =========="

  echo "$((i+1)). Deleting sts"
  kubectl --kubeconfig=$kubeconfig -n $namespace delete statefulset $sts_name --cascade=orphan --wait=true

  echo "$((i+1)). Deleting replica"
  kubectl --kubeconfig=$kubeconfig -n $namespace delete pod $sts_name-$i --wait=true

  echo "$((i+1)). Upgrading Helm"
  # The "--wait" flag ensures the deleted pod replica and STS are up and running.
  # However, at times, the STS fails to understand that all replicas are running and the upgrade is stuck.
  # The "--timeout 1m" helps with short-circuiting the upgrade process. Even if the upgrade does time out, it is
  # harmless and the last upgrade process will be successful once all the pods replicas have been updated.
  helm upgrade -f $values_file $release_name $chart --kubeconfig=$kubeconfig --namespace $namespace --version $chart_version --wait --timeout 1m --debug

  echo "Iteration $((i+1)) complete. Kindly validate the changes before proceeding."
  read -p "Press enter to continue ..."
done
