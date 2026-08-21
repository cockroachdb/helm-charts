#!/usr/bin/env bash
set -euo pipefail

subscription_id="${AZURE_SUBSCRIPTION_ID:-}"
if [[ -z "${subscription_id}" ]]; then
  echo "AZURE_SUBSCRIPTION_ID is required" >&2
  exit 1
fi

debug_dir="${AZURE_DEBUG_DIR:-${RUNNER_TEMP:-/tmp}/azure-debug}"
mkdir -p "${debug_dir}"

resource_groups_file="${AZURE_RESOURCE_GROUPS_FILE:-${RUNNER_TEMP:-/tmp}/azure-resource-groups.txt}"
resource_prefix="${AZURE_RESOURCE_PREFIX:-helm-charts-e2e}"
ticket="${AZURE_TICKET:-}"
current_run_id="${GITHUB_RUN_ID:-}"
current_workflow="${GITHUB_WORKFLOW:-}"
current_job="${GITHUB_JOB:-}"
current_ref_name="${GITHUB_REF_NAME:-}"
cleanup_wait_seconds="${AZURE_CLEANUP_WAIT_SECONDS:-3600}"

timestamp() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

run_az() {
  echo "+ az $*"
  az "$@" || true
}

delete_resource_group_async() {
  local rg="$1"
  local output

  echo "Deleting Azure resource group ${rg}"
  if output="$(az group delete \
    --name "${rg}" \
    --subscription "${subscription_id}" \
    --yes \
    --no-wait \
    --output none 2>&1)"; then
    [[ -n "${output}" ]] && echo "${output}"
    return 0
  fi

  if [[ "${output}" == *"ResourceGroupNotFound"* || "${output}" == *"could not be found"* || "${output}" == *"was not found"* ]]; then
    echo "Azure resource group ${rg} is already deleted"
    return 0
  fi

  echo "Failed to start async deletion for Azure resource group ${rg}: ${output}" >&2
  return 1
}

resource_groups() {
  [[ -f "${resource_groups_file}" ]] || return 0
  sed '/^[[:space:]]*$/d' "${resource_groups_file}" | sort -u
}

old_resource_groups() {
  if [[ -z "${ticket}" ]]; then
    echo "AZURE_TICKET is required for old artifact cleanup" >&2
    return 1
  fi

  local query="[?starts_with(name, '${resource_prefix}-rg-') && tags.ManagedBy=='helm-charts-e2e' && tags.Ticket=='${ticket}'"
  if [[ -n "${current_run_id}" ]]; then
    query+=" && tags.GitHubRunID!='${current_run_id}'"
  fi
  if [[ -n "${current_workflow}" ]]; then
    query+=" && tags.GitHubWorkflow=='${current_workflow}'"
  fi
  if [[ -n "${current_job}" ]]; then
    query+=" && tags.GitHubJob=='${current_job}'"
  fi
  if [[ -n "${current_ref_name}" ]]; then
    query+=" && tags.GitHubRefName=='${current_ref_name}'"
  fi
  query+="].name"

  az group list \
    --subscription "${subscription_id}" \
    --query "${query}" \
    --output tsv
}

resource_group_exists() {
  local rg="$1"
  local exists
  local stderr_file
  stderr_file="$(mktemp)"
  if ! exists="$(az group exists --name "${rg}" --subscription "${subscription_id}" --output tsv 2>"${stderr_file}")"; then
    local err_output
    err_output="$(cat "${stderr_file}")"
    rm -f "${stderr_file}"
    echo "Failed to check Azure resource group ${rg}: ${err_output}" >&2
    return 2
  fi
  rm -f "${stderr_file}"

  case "${exists}" in
    true)
      return 0
      ;;
    false)
      return 1
      ;;
    *)
      echo "Unexpected Azure resource group existence response for ${rg}: ${exists}" >&2
      return 2
      ;;
  esac
}

wait_resource_group_deleted() {
  local rg="$1"
  local deadline=$((SECONDS + cleanup_wait_seconds))

  while true; do
    if resource_group_exists "${rg}"; then
      if (( SECONDS >= deadline )); then
        echo "Timed out waiting for Azure resource group ${rg} to be deleted" >&2
        return 1
      fi
      echo "Waiting for Azure resource group ${rg} deletion to finish"
      sleep 30
      continue
    fi

    local exists_status="$?"
    if [[ "${exists_status}" == "1" ]]; then
      echo "Azure resource group ${rg} deletion finished"
      return 0
    fi
    return "${exists_status}"
  done
}

snapshot_resource_group() {
  local rg="$1"
  echo "Resource group: ${rg}"
  echo

  echo "Resource group details:"
  run_az group show \
    --name "${rg}" \
    --subscription "${subscription_id}" \
    --output table
  echo

  echo "Resources:"
  run_az resource list \
    --resource-group "${rg}" \
    --subscription "${subscription_id}" \
    --query "[].{name:name,type:type,location:location,provisioningState:properties.provisioningState}" \
    --output table
  echo

  echo "AKS clusters:"
  run_az aks list \
    --resource-group "${rg}" \
    --subscription "${subscription_id}" \
    --query "[].{name:name,location:location,kubernetesVersion:kubernetesVersion,provisioningState:provisioningState,powerState:powerState.code,nodeResourceGroup:nodeResourceGroup}" \
    --output table
  echo

  echo "Virtual networks:"
  run_az network vnet list \
    --resource-group "${rg}" \
    --subscription "${subscription_id}" \
    --query "[].{name:name,location:location,addressPrefixes:addressSpace.addressPrefixes}" \
    --output table
  echo

  local vnet
  while IFS= read -r vnet; do
    [[ -n "${vnet}" ]] || continue
    echo "VNet peerings for ${vnet}:"
    run_az network vnet peering list \
      --resource-group "${rg}" \
      --vnet-name "${vnet}" \
      --subscription "${subscription_id}" \
      --query "[].{name:name,provisioningState:provisioningState,peeringState:peeringState,remoteVirtualNetwork:remoteVirtualNetwork.id}" \
      --output table
    echo
  done < <(az network vnet list \
    --resource-group "${rg}" \
    --subscription "${subscription_id}" \
    --query "[].name" \
    --output tsv 2>/dev/null || true)

  echo "Public IPs:"
  run_az network public-ip list \
    --resource-group "${rg}" \
    --subscription "${subscription_id}" \
    --query "[].{name:name,location:location,ipAddress:ipAddress,provisioningState:provisioningState,associatedTo:ipConfiguration.id}" \
    --output table
  echo

  echo "Load balancers:"
  run_az network lb list \
    --resource-group "${rg}" \
    --subscription "${subscription_id}" \
    --query "[].{name:name,location:location,provisioningState:provisioningState,frontendIPConfigurations:frontendIPConfigurations[].name}" \
    --output table
  echo

  echo "Disks:"
  run_az disk list \
    --resource-group "${rg}" \
    --subscription "${subscription_id}" \
    --query "[].{name:name,location:location,diskSizeGb:diskSizeGb,provisioningState:provisioningState,managedBy:managedBy}" \
    --output table
  echo

  echo "Recent failed activity log events:"
  run_az monitor activity-log list \
    --resource-group "${rg}" \
    --subscription "${subscription_id}" \
    --status Failed \
    --max-events 80 \
    --query "[].{eventTimestamp:eventTimestamp,operationName:operationName.localizedValue,status:status.localizedValue,subStatus:subStatus.localizedValue,resourceGroupName:resourceGroupName,resourceProviderName:resourceProviderName.localizedValue,resourceId:resourceId,claims:claims.name}" \
    --output table
  echo
}

snapshot() {
  local label="${1:-snapshot}"
  local file="${debug_dir}/azure-${label}-$(date -u +"%Y%m%dT%H%M%SZ").log"

  {
    echo "Azure CI debug snapshot: ${label}"
    echo "Timestamp: $(timestamp)"
    echo "Subscription: ${subscription_id}"
    echo "Resource prefix: ${resource_prefix}"
    echo "Ticket: ${ticket:-unset}"
    echo "Current GitHub run ID: ${current_run_id:-unset}"
    echo "Current GitHub workflow: ${current_workflow:-unset}"
    echo "Current GitHub job: ${current_job:-unset}"
    echo "Current GitHub ref name: ${current_ref_name:-unset}"
    echo "Resource groups file: ${resource_groups_file}"
    echo

    echo "Azure CLI account:"
    run_az account show \
      --subscription "${subscription_id}" \
      --query "{name:name,id:id,tenantId:tenantId,user:user.name}" \
      --output table
    echo

    echo "Recorded resource groups:"
    resource_groups
    echo

    echo "Known helm-charts Azure resource groups:"
    run_az group list \
      --subscription "${subscription_id}" \
      --query "[?starts_with(name, '${resource_prefix}-rg-') || tags.ManagedBy=='helm-charts-e2e'].{name:name,location:location,provisioningState:properties.provisioningState,ticket:tags.Ticket,testRun:tags.TestRun}" \
      --output table
    echo

    local rg
    while IFS= read -r rg; do
      [[ -n "${rg}" ]] || continue
      snapshot_resource_group "${rg}"
    done < <(resource_groups)
  } 2>&1 | tee "${file}"
}

watch() {
  local interval="${1:-120}"
  while true; do
    snapshot "periodic"
    sleep "${interval}"
  done
}

cleanup_resource_groups() {
  if [[ "${AZURE_SKIP_TEARDOWN:-false}" == "true" ]]; then
    echo "AZURE_SKIP_TEARDOWN=true; preserving recorded Azure resource groups"
    return 0
  fi

  mapfile -t groups < <(resource_groups)
  if (( ${#groups[@]} == 0 )); then
    echo "No recorded Azure resource groups found; skipping cleanup"
    return 0
  fi

  local cleanup_status=0
  local rg
  for rg in "${groups[@]}"; do
    [[ -n "${rg}" ]] || continue
    delete_resource_group_async "${rg}" || cleanup_status="$?"
  done
  return "${cleanup_status}"
}

cleanup_old_resource_groups() {
  if [[ "${AZURE_CLEANUP_OLD_RESOURCE_GROUPS:-true}" != "true" ]]; then
    echo "AZURE_CLEANUP_OLD_RESOURCE_GROUPS is not true; skipping old artifact cleanup"
    return 0
  fi
  if [[ -z "${ticket}" ]]; then
    echo "AZURE_TICKET is required for old artifact cleanup" >&2
    return 1
  fi

  local old_groups_output
  if ! old_groups_output="$(old_resource_groups)"; then
    return 1
  fi

  local groups=()
  if [[ -n "${old_groups_output}" ]]; then
    mapfile -t groups <<< "${old_groups_output}"
  fi

  if (( ${#groups[@]} == 0 )); then
    echo "No old Azure resource groups found for prefix ${resource_prefix} and ticket ${ticket}"
    return 0
  fi

  echo "Found old Azure resource groups for prefix ${resource_prefix} and ticket ${ticket}:"
  printf '  %s\n' "${groups[@]}"
  echo "Submitting async cleanup for old Azure resource groups, then failing this run before provisioning new infrastructure."

  local cleanup_status=1
  local rg
  for rg in "${groups[@]}"; do
    [[ -n "${rg}" ]] || continue
    delete_resource_group_async "${rg}" || cleanup_status="$?"
  done

  return "${cleanup_status}"
}

case "${1:-}" in
  snapshot)
    snapshot "${2:-snapshot}"
    ;;
  watch)
    watch "${2:-120}"
    ;;
  cleanup-resource-groups)
    cleanup_resource_groups
    ;;
  cleanup-old-resource-groups)
    cleanup_old_resource_groups
    ;;
  *)
    echo "usage: $0 {snapshot [label]|watch [seconds]|cleanup-resource-groups|cleanup-old-resource-groups}" >&2
    exit 2
    ;;
esac
