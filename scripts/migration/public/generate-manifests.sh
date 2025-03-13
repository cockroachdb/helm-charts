#!/usr/bin/env bash

set -euo pipefail
set -x

# Define YAML files
SOURCE_FILE="backup/crdbcluster-$CRDBCLUSTER.yaml"
STS_FILE="backup/sts-$CRDBCLUSTER.yaml"
TARGET_FILE="custom_values.yaml"
NODE_TEMPLATE_FILE="crdbnode_template.yaml"
ENV_SUBST_CMD=" (.. | select(tag == \"!!str\")) |= envsubst |"

# Start building the yq command dynamically
YQ_CMD="yq '"
NODE_TEMPLATE_YQ_CMD="yq '"

kubectl get sts -o yaml $CRDBCLUSTER > $STS_FILE
num_nodes=$(cat "${STS_FILE}" | yq '.spec.replicas')

export join_str=""
for idx in $(seq 0 $(($num_nodes - 1))); do
  if [[ -n "${join_str}" ]]; then
    join_str="${join_str},"
  fi
  export join_str="${join_str}${CRDBCLUSTER}-${idx}.${CRDBCLUSTER}.${NAMESPACE}:26258"
done

# Function to check if a field exists in source.yaml and append to YQ_CMD
append_if_exists() {
    local field_path=$1
    local target_path=$2

    val=$(yq "$field_path" "$SOURCE_FILE")
    if [[ -n "$target_path" && "$val" != "null" && "$val" != "" ]]; then
        YQ_CMD+=" $target_path = load(\"$SOURCE_FILE\")$field_path |"
    fi
}

append_to_node_cmd_if_exists() {
    local field_path=$1
    local target_path=$2

    val=$(yq "$field_path" "$SOURCE_FILE")
    if [[ -n "$target_path" && "$val" != "null" && "$val" != "" ]]; then
        NODE_TEMPLATE_YQ_CMD+=" $target_path = load(\"$SOURCE_FILE\")$field_path |"
    fi
}

# Function to append YQ_CMD with source and target path
append_in_yq() {
    local source_path=$1
    local target_path=$2

    if [[ -n "$target_path" ]]; then
      YQ_CMD+=" $target_path = $source_path |"
    fi
}

append_in_node_cmd() {
    local source_path=$1
    local target_path=$2

    if [[ -n "$target_path" ]]; then
        NODE_TEMPLATE_YQ_CMD+=" $target_path = $source_path |"
    fi
}

# Special Handling for specific fields

# terminationGracePeriodSecs takes input as int in public operator which specifies the number of seconds.
# In self hosted operator, terminationGracePeriod input is of metav1.Duration which is taken as string like
# "300s" or "5m". This has to be handled while generating the templates.
append_in_yq_terminationGracePeriodSecs() {
    local field_path=$1
    local target_path=$2

    val=$(yq "$field_path" "$SOURCE_FILE")
    if [[ -n "$target_path" && "$val" != "null" && "$val" != "" ]]; then
        YQ_CMD+=" $target_path = \"${val}s\" |"
    fi
}

append_in_node_yq_terminationGracePeriodSecs() {
    local field_path=$1
    local target_path=$2

    val=$(yq "$field_path" "$SOURCE_FILE")
    if [[ -n "$target_path" && "$val" != "null" && "$val" != "" ]]; then
        NODE_TEMPLATE_YQ_CMD+=" $target_path += \"${val}s\" |"
    fi
}

# podEnvVariables needs to be append to the existing environment variable present on the crdbnode.
# There needs to be existing "HOST_IP" env variables which should be present and the user provided
# env variables needs to be appended into it.

append_in_node_yq_env() {
    local field_path=$1
    local target_path=$2

    val=$(yq "$field_path" "$SOURCE_FILE")
    val=$(yq "$field_path" "$SOURCE_FILE")
    if [[ -n "$target_path" && "$val" != "null" && "$val" != "" ]]; then
        NODE_TEMPLATE_YQ_CMD+=" $target_path += ${val}s |"
    fi
}

# Append fields only if they exist

append_if_exists ".spec.tlsEnabled" ".operator.tlsEnabled"
append_if_exists ".spec.resources" ".operator.resources"
append_if_exists ".spec.additionalAnnotations" ".operator.podAnnotations"
append_if_exists ".spec.grpcPort" ".operator.ports.grpcPort"
append_if_exists ".spec.httpPort" ".operator.ports.httpPort"
append_if_exists ".spec.sqlPort" ".operator.ports.sqlPort"
append_if_exists ".spec.image.name" ".operator.image.name"
append_if_exists ".spec.image.pullPolicy" ".operator.image.pullPolicy"
append_if_exists ".spec.dataStore.pvc.spec" ".operator.dataStore.volumeClaimTemplate.spec"
append_if_exists ".spec.dataStore.pvc.source.claimName" ".operator.dataStore.dataStore.persistentVolumeClaim"
append_if_exists ".spec.dataStore.hostPath" ".operator.dataStore.dataStore.hostPath"
append_if_exists ".spec.podEnvVariables" ".operator.env"
append_if_exists ".spec.tolerations" ".operator.tolerations"
append_if_exists ".spec.topologySpreadConstraints" ".operator.topologySpreadConstraints"
append_if_exists ".spec.nodeSelector" ".operator.nodeSelector"
append_in_yq_terminationGracePeriodSecs ".spec.terminationGracePeriodSecs" ".operator.terminationGracePeriod"
append_if_exists ".spec.cache" ".operator.flags.--cache"
append_if_exists ".spec.maxSQLMemory" ".operator.flags.\"--max-sql-memory\""
append_if_exists ".spec.logConfigMap" ".operator.loggingConfigMapName"
append_if_exists ".spec.nodes" ".operator.regions[0].nodes"

# Append fields in yq
append_in_yq "load(\"$STS_FILE\").spec.template.metadata.labels" ".spec.podLabels"
append_in_yq "env(NAMESPACE)" ".operator.regions[0].namespace"
append_in_yq "env(CLOUD_PROVIDER)" ".operator.regions[0].cloudProvider"
append_in_yq "env(REGION)" ".operator.regions[0].code"

append_in_yq "(env(CRDBCLUSTER) + \"-ca\")" ".operator.certificates.externalCertificates.caConfigMapName" ".spec.certificates.externalCertificates.caConfigMapName"
append_in_yq "(env(CRDBCLUSTER) + \"-node-certs\")" ".operator.certificates.externalCertificates.nodeSecretName" ".spec.certificates.externalCertificates.nodeSecretName"
append_in_yq "(env(CRDBCLUSTER) + \"-client-certs\")" ".operator.certificates.externalCertificates.rootSqlClientSecretName" ".spec.certificates.externalCertificates.rootSqlClientSecretName"

for idx in $(seq 0 $(($num_nodes - 1))); do
  export crdb_node_name=${CRDBCLUSTER}-${idx}
  export k8s_node_name=$(kubectl get pod -o yaml ${crdb_node_name} | yq '.spec.nodeName')

  NODE_TEMPLATE_YQ_CMD="yq '"

  append_to_node_cmd_if_exists ".spec.resources" ".spec.resourceRequirements"
  append_to_node_cmd_if_exists ".spec.additionalAnnotations" ".spec.podAnnotations"
  append_to_node_cmd_if_exists ".spec.grpcPort" ".spec.grpcPort"
  append_to_node_cmd_if_exists ".spec.httpPort" ".spec.httpPort"
  append_to_node_cmd_if_exists ".spec.sqlPort" ".spec.sqlPort"
  append_to_node_cmd_if_exists ".spec.image.name" ".spec.image"
  append_to_node_cmd_if_exists ".spec.dataStore.pvc.spec" ".spec.dataStore.volumeClaimTemplate.spec"
  append_to_node_cmd_if_exists ".spec.dataStore.pvc.source.claimName" ".spec.dataStore.dataStore.persistentVolumeClaim"
  append_to_node_cmd_if_exists ".spec.dataStore.hostPath" ".spec.dataStore.dataStore.hostPath"
  append_in_node_yq_env ".spec.podEnvVariables" ".spec.env"
  append_to_node_cmd_if_exists ".spec.tolerations" ".spec.tolerations"
  append_to_node_cmd_if_exists ".spec.topologySpreadConstraints" ".spec.topologySpreadConstraints"
  append_to_node_cmd_if_exists ".spec.nodeSelector" ".spec.nodeSelector"
  append_in_node_yq_terminationGracePeriodSecs ".spec.terminationGracePeriodSecs" ".spec.terminationGracePeriod"
  append_to_node_cmd_if_exists ".spec.cache" ".spec.flags.--cache"
  append_to_node_cmd_if_exists ".spec.maxSQLMemory" ".spec.flags.\"--max-sql-memory\""
  append_to_node_cmd_if_exists ".spec.logConfigMap" ".spec.loggingConfigMapName"

  append_in_node_cmd "load(\"$STS_FILE\").spec.template.metadata.labels" ".spec.podLabels" ".spec.podLabels"
  append_in_node_cmd "(env(CRDBCLUSTER) + \"-ca\")" ".spec.certificates.externalCertificates.caConfigMapName"
  append_in_node_cmd "(env(CRDBCLUSTER) + \"-node-certs\")" ".spec.certificates.externalCertificates.nodeSecretName"
  append_in_node_cmd "(env(CRDBCLUSTER) + \"-client-certs\")" ".spec.certificates.externalCertificates.rootSqlClientSecretName"
  append_in_node_cmd "(env(k8s_node_name))" ".spec.nodeName"
  append_in_node_cmd "\"$join_str\"" ".spec.join"

  NODE_TEMPLATE_YQ_CMD+="${ENV_SUBST_CMD}"
  NODE_TEMPLATE_YQ_CMD=${NODE_TEMPLATE_YQ_CMD%"|"}
  NODE_TEMPLATE_YQ_CMD+=" ' $NODE_TEMPLATE_FILE > manifests/crdbnode-${CRDBCLUSTER}-${idx}.yaml"
  echo "\n"
  echo "Generating template file for crdbnode ${CRDBCLUSTER}-${idx} as manifests/crdbnode-${CRDBCLUSTER}-${idx}.yaml \n"
  eval $NODE_TEMPLATE_YQ_CMD
done


# Remove the last `|` and close the command
echo "\n Generating values.yaml file for helm chart installation of cockroachdb in custom_values.yaml"
YQ_CMD+="${ENV_SUBST_CMD}"
YQ_CMD=${YQ_CMD%"|"}
YQ_CMD+=" ' $TARGET_FILE > manifests/$TARGET_FILE"

# Execute the dynamically built command
eval "$YQ_CMD"
