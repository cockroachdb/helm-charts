#!/usr/bin/env bash
# Discover stale test resources, then reuse the provider teardown helpers.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${script_dir}/common.sh"
cleanup_parse_args "$@"
set -- "${CLEANUP_ARGS[@]}"
provider="${1:-}"
[[ $# -gt 0 ]] && shift
max_age_hours=24
while [[ $# -gt 0 ]]; do
  case "$1" in
    --max-age-hours)
      max_age_hours="${2:?age is required}"
      shift 2
      ;;
    *)
      echo "Unknown argument: $1" >&2
      exit 2
      ;;
  esac
done
if [[ "${provider}" != "gcp" && "${provider}" != "azure" ]]; then
  echo "Usage: $0 gcp|azure [--dry-run] [--max-age-hours HOURS]" >&2
  exit 2
fi
if [[ ! "${max_age_hours}" =~ ^[0-9]+$ || "${max_age_hours}" -lt 24 ]]; then
  echo "Minimum resource age is 24 hours" >&2
  exit 2
fi
repository="${GITHUB_REPOSITORY:-cockroachdb/helm-charts}"
[[ "${repository}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || exit 2
cutoff="$(( $(date +%s) - max_age_hours * 3600 ))"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/installs"
export KUBECONFIG="${work}/kubeconfig"
status=0
touch "${work}/selected.jsonl"

policy() {
  jq -L "${script_dir}" "$@"
}

is_protected() {
  policy -ne --arg name "$1" 'include "policy"; {name:$name} | protected' >/dev/null
}

run_completed() {
  local run="$1"
  [[ "${run}" =~ ^[0-9]+$ ]] || return 1
  # Stop discovery if run status cannot be checked.
  if [[ ! -f "${work}/run-${run}.json" ]]; then
    gh api "repos/${repository}/actions/runs/${run}" > "${work}/run-${run}.json" || exit 1
  fi
  jq -e --arg repo "${repository}" '.status == "completed" and .repository.full_name == $repo' "${work}/run-${run}.json" >/dev/null
}

eligible() {
  local metadata="$1" kind="$2" project="${3:-}"
  if ! policy -e --arg repo "${repository}" --arg provider "${kind}" --arg project "${project}" --argjson cutoff "${cutoff}" '
    include "policy";
    .managed_by == "helm-charts-e2e" and .repository == $repo and .provider == $provider and
    ($project == "" or .project == $project) and (.created_at | old($cutoff))' <<< "${metadata}" >/dev/null; then
    return 1
  fi
  run_completed "$(jq -r '.run_id' <<< "${metadata}")"
}

# Resources without run IDs are considered only while the repository is quiet.
repository_quiet() {
  local run_status page runs
  if [[ -f "${work}/busy" ]]; then
    return 1
  fi
  if [[ -f "${work}/quiet" ]]; then
    return 0
  fi
  for run_status in in_progress queued waiting requested pending; do
    page=1
    while true; do
      runs="$(gh api "repos/${repository}/actions/runs?status=${run_status}&per_page=100&page=${page}")" || exit 1
      if ! jq -e --arg own "${GITHUB_RUN_ID:-}" 'all(.workflow_runs[]; (.id | tostring) == $own)' <<< "${runs}" >/dev/null; then
        echo "SKIP resources without run IDs: ${run_status} workflow runs exist" >&2
        touch "${work}/busy"
        return 1
      fi
      [[ "$(jq '.workflow_runs | length' <<< "${runs}")" -lt 100 ]] && break
      page=$((page + 1))
    done
  done
  touch "${work}/quiet"
}

prepare_openshift() {
  local infra="$1" metadata="$2"
  if [[ ! "${infra}" =~ ^ocp-[a-f0-9]{10}-[a-z0-9]{5}$ ]]; then
    echo "Invalid OpenShift infra ID" >&2
    return 1
  fi
  if is_protected "${infra}"; then
    echo "SKIP ${infra}: protected"
    return 0
  fi
  jq -e --arg infra "${infra}" --arg project "${GCP_PROJECT_ID}" '
    .infraID == $infra and .gcp.projectID == $project and
    (.gcp.region | type == "string" and test("^[a-z]+-[a-z]+[0-9]+$"))' <<< "${metadata}" >/dev/null || {
    echo "OpenShift metadata does not match its infra ID/project/region" >&2
    return 1
  }
  mkdir -p "${work}/installs/${infra}"
  printf '%s\n' "${metadata}" > "${work}/installs/${infra}/metadata.json"
}

if [[ "${provider}" == "azure" ]]; then
  : "${AZURE_SUBSCRIPTION_ID:?AZURE_SUBSCRIPTION_ID is required}"
  groups="$(az group list --subscription "${AZURE_SUBSCRIPTION_ID}" --output json)"
  while IFS= read -r group; do
    name="$(jq -r '.name' <<< "${group}")"
    [[ "${name}" =~ ^(hc-e2e|helm-charts-e2e)-rg-[a-z0-9-]+$ ]] || continue
    if policy -e 'include "policy"; protected' <<< "${group}" >/dev/null; then
      echo "SKIP ${name}: protected"
      continue
    fi
    metadata="$(jq -c '{managed_by:.tags.ManagedBy,repository:.tags.GitHubRepository,run_id:.tags.GitHubRunID,created_at:.tags.CreatedAtUnix,provider:"azure"}' <<< "${group}")"
    # Older groups already have ownership/run tags, but lack CreatedAtUnix.
    if jq -e --arg repo "${repository}" '.managed_by == "helm-charts-e2e" and .repository == $repo and .created_at == null' <<< "${metadata}" >/dev/null; then
      run="$(jq -r '.run_id' <<< "${metadata}")"
      if run_completed "${run}"; then
        created="$(policy -r 'include "policy"; .updated_at | epoch' "${work}/run-${run}.json")"
        metadata="$(jq -c --arg created "${created}" '.created_at=$created' <<< "${metadata}")"
      fi
    fi
    if eligible "${metadata}" azure; then
      jq -cn --arg name "${name}" '{type:"resource-group",name:$name}' >> "${work}/selected.jsonl"
    fi
  done < <(jq -c '.[]' <<< "${groups}")
else
  : "${GCP_PROJECT_ID:?GCP_PROJECT_ID is required}"
  : "${GCP_CLEANUP_STATE_URI:?GCP_CLEANUP_STATE_URI is required}"
  [[ "${GCP_CLEANUP_STATE_URI}" =~ ^gs://[a-z0-9._-]+/cleanup$ ]] || exit 2
  source "${script_dir}/discover-gcp.sh"
  discover_gcp
fi
cleanup_header "${repository}, ${provider}, minimum age ${max_age_hours} hours"
cleanup_report selected-resource "$(jq -s '.' "${work}/selected.jsonl")"
if [[ "${provider}" == "gcp" ]]; then
  while IFS= read -r dir; do
    cleanup_report openshift-install "$(jq -c '[{name:.infraID,id:.infraID,location:.gcp.region}]' "${dir}/metadata.json")"
  done < <(find "${work}/installs" -mindepth 1 -maxdepth 1 -type d)
  # Keep recovered metadata until destruction succeeds, even if this runner exits.
  while IFS= read -r dir; do
    infra="$(jq -er '.infraID' "${dir}/metadata.json")"
    uri="${GCP_CLEANUP_STATE_URI}/recovered-installs/${infra}/metadata.json"
    cleanup_change gcloud storage cp "${dir}/metadata.json" "${uri}"
    printf '%s\n' "${uri}" >> "${work}/retire-state"
  done < <(find "${work}/installs" -mindepth 1 -maxdepth 1 -type d)
fi
while IFS= read -r resource; do
  name="$(jq -r '.name' <<< "${resource}")"
  case "$(jq -r '.type' <<< "${resource}")" in
    resource-group)
      bash "${script_dir}/azure.sh" resource-group "${name}" || status=1
      ;;
    network)
      GCP_CLEANUP_EXPECTED_CLUSTERS_JSON="$(jq -c '.clusters' <<< "${resource}")" \
      bash "${script_dir}/gcp.sh" network "${name}" "$(jq -r '.id' <<< "${resource}")" || status=1
      ;;
    service-account)
      bash "${script_dir}/openshift.sh" service-account "${name}" "$(jq -r '.id' <<< "${resource}")" "$(jq -r '.infra' <<< "${resource}")" || status=1
      ;;
    observation)
      printf '%s\n' "${resource}" > "${work}/observation.json"
      cleanup_change gcloud storage cp "${work}/observation.json" "$(jq -r '.uri' <<< "${resource}")" || status=1
      ;;
  esac
done < "${work}/selected.jsonl"
if [[ "${provider}" == "gcp" ]]; then
  if OPENSHIFT_INSTALL_DIRS_ROOT="${work}/installs" bash "${script_dir}/openshift.sh" recorded-installs; then
    if [[ -f "${work}/retire-state" ]]; then
      while IFS= read -r uri; do
        cleanup_change gcloud storage rm "${uri}" || status=1
      done < "${work}/retire-state"
    fi
  else
    status=1
  fi
  # Re-discover after cluster deletion, when persistent disks become unattached.
  if repository_quiet; then
    bash "${script_dir}/gcp.sh" leftovers "${cutoff}" || status=1
  fi
fi
exit "${status}"
