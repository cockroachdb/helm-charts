#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
source "$(dirname "${BASH_SOURCE[0]}")/../lib/cloud-resources.sh"
cleanup_parse_args "$@"
set -- "${CLEANUP_ARGS[@]}"
subscription_id="${AZURE_SUBSCRIPTION_ID:?AZURE_SUBSCRIPTION_ID is required}"

delete_resource_group() {
  local rg="$1"
  local output

  if output="$(cleanup_change az group delete \
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

report_resource_group() {
  local rg="$1" resources clusters node_group
  cleanup_report resource-group "$(jq -cn --arg name "${rg}" '[{name:$name}]')"
  if ! resources="$(az resource list --resource-group "${rg}" --subscription "${subscription_id}" --output json 2>&1)"; then
    if [[ "${resources}" == *"ResourceGroupNotFound"* ]]; then
      echo "Resource group ${rg} is already deleted"
      return 0
    fi
    echo "${resources}" >&2
    return 1
  fi
  cleanup_report resource "${resources}"
  if ! clusters="$(az aks list --resource-group "${rg}" --subscription "${subscription_id}" --output json 2>&1)"; then
    [[ "${clusters}" == *"ResourceGroupNotFound"* ]] && return 0
    echo "${clusters}" >&2
    return 1
  fi
  while IFS= read -r node_group; do
    cleanup_report node-resource-group "$(jq -cn --arg name "${node_group}" '[{name:$name}]')"
    resources="$(az resource list --resource-group "${node_group}" --subscription "${subscription_id}" --output json)"
    cleanup_report resource "${resources}"
  done < <(jq -r '[.[].nodeResourceGroup // empty] | unique[]' <<< "${clusters}")
}

groups=()
case "${1:-}" in
  resource-group)
    if [[ $# -ne 2 ]]; then
      echo "Usage: $0 resource-group NAME [--dry-run]" >&2
      exit 2
    fi
    groups+=("$2")
    ;;
  recorded-resource-groups)
    [[ $# -eq 1 ]] || exit 2
    while IFS= read -r group; do
      groups+=("${group}")
    done < <(resource_groups)
    ;;
  *)
    echo "Usage: $0 resource-group NAME | recorded-resource-groups [--dry-run]" >&2
    exit 2
    ;;
esac

cleanup_header "Azure subscription ${subscription_id}"
for group in "${groups[@]}"; do
  report_resource_group "${group}"
done
status=0
for group in "${groups[@]}"; do
  if delete_resource_group "${group}"; then
    if [[ "${CLEANUP_DRY_RUN}" != "true" ]]; then
      az group wait --deleted --name "${group}" --subscription "${subscription_id}" --interval 15 --timeout 2700 || status=1
    fi
  else
    status=1
  fi
done
exit "${status}"
