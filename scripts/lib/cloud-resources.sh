#!/usr/bin/env bash
# Local resource records shared by diagnostics and cleanup.

install_dirs() {
  if [[ -n "${OPENSHIFT_INSTALL_DIRS:-}" ]]; then
    printf '%s\n' "${OPENSHIFT_INSTALL_DIRS}"
    return
  fi
  if [[ -n "${OPENSHIFT_INSTALL_DIRS_ROOT:-}" ]]; then
    find "${OPENSHIFT_INSTALL_DIRS_ROOT}" -mindepth 1 -maxdepth 1 -type d | sort
    return
  fi
  local tmpdir="${TMPDIR:-${RUNNER_TEMP:-/tmp}}"
  find "${tmpdir}" -maxdepth 1 -type d -name "helm-charts-ocp-*" 2>/dev/null | sort || true
}

infra_ids() {
  local install_dir metadata infra_id
  while IFS= read -r install_dir; do
    metadata="${install_dir}/metadata.json"
    [[ -f "${metadata}" ]] || continue
    infra_id="$(jq -er '.infraID' "${metadata}")" || return 1
    [[ "${infra_id}" =~ ^ocp-[a-z0-9-]+$ ]] || { echo "Invalid OpenShift infra ID" >&2; return 1; }
    [[ -n "${infra_id}" ]] || continue
    printf "%s\n" "${infra_id}"
  done < <(install_dirs)
}

resource_groups() {
  [[ -f "${AZURE_RESOURCE_GROUPS_FILE:-${RUNNER_TEMP:-/tmp}/azure-resource-groups.txt}" ]] || return 0
  sed '/^[[:space:]]*$/d' "${AZURE_RESOURCE_GROUPS_FILE:-${RUNNER_TEMP:-/tmp}/azure-resource-groups.txt}" | sort -u
}
