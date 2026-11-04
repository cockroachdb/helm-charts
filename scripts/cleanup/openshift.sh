#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
source "$(dirname "${BASH_SOURCE[0]}")/../lib/cloud-resources.sh"
cleanup_parse_args "$@"
set -- "${CLEANUP_ARGS[@]}"
project_id="${GCP_PROJECT_ID:-}"

multicluster_plan() {
  local found ids
  found="$(infra_ids)" || return 1
  ids="$(jq -Rsc 'split("\n") | map(select(length > 0)) | unique' <<< "${found}")"
  jq -cn --argjson ids "${ids}" '
    [$ids[] as $id | ["submariner-public-ports-ingress", "submariner-internal-ports-ingress"][] |
      {type:"firewall-rules", name:($id + "-" + .)}] +
    [range(0; $ids | length) as $i | range($i + 1; $ids | length) as $j |
      [$ids[$i], $ids[$j]] | (., reverse) | .[0] as $a | .[1] as $b |
      ($a | split("-")[:2] | join("-")) as $short_a |
      ($b | split("-")[:2] | join("-")) as $short_b |
      {type:"firewall-rules", name:($short_a + "-allow-peer-" + $short_b)},
      {type:"peerings", name:($short_a + "-to-" + $short_b), network:($a + "-network")} ]'
}

report_install() {
  local dir="$1" metadata infra project family resources
  metadata="$(cat "${dir}/metadata.json")"
  infra="$(jq -er '.infraID' <<< "${metadata}")"
  [[ "${infra}" =~ ^ocp-[a-z0-9-]+$ ]] || { echo "Invalid OpenShift infra ID" >&2; return 1; }
  project="$(jq -r '.gcp.projectID // empty' <<< "${metadata}")"
  cleanup_report openshift-install "$(jq -c '[{name:.infraID, id:.infraID, location:(.gcp.region // "-")}]' <<< "${metadata}")"
  [[ -n "${project}" ]] || return 0
  for family in instances disks forwarding-rules backend-services target-pools health-checks http-health-checks addresses firewall-rules routers routes networks 'networks subnets'; do
    local parts=()
    read -r -a parts <<< "${family}"
    resources="$(gcloud compute "${parts[@]}" list --project="${project}" --filter="name~'^${infra}-'" --format=json)"
    cleanup_report "${family}" "${resources}"
  done
}

delete_if_exists() {
  local output
  if output="$(cleanup_change gcloud "$@" 2>&1)"; then
    echo "${output}"
  elif ! cleanup_missing "${output}"; then
    echo "${output}" >&2
    return 1
  fi
}

destroy_install() {
  local dir="$1" state_uri="" infra
  if [[ -f "${dir}/ci-cleanup.json" && -n "${OPENSHIFT_CLEANUP_GCS_URI:-}" ]]; then
    infra="$(jq -er '.infra_id' "${dir}/ci-cleanup.json")" || return 1
    [[ "${infra}" =~ ^ocp-[a-z0-9-]+$ && "${OPENSHIFT_CLEANUP_GCS_URI}" =~ ^gs://[a-z0-9._-]+/cleanup$ ]] || return 1
    state_uri="${OPENSHIFT_CLEANUP_GCS_URI}/${infra}"
  fi
  if ! cleanup_change openshift-install destroy cluster --dir "${dir}" --log-level info; then
    echo "Cluster deletion failed; retaining installer state at ${dir}" >&2
    return 1
  fi
  if [[ -n "${state_uri}" ]]; then
    delete_if_exists storage rm "${state_uri}/ci-cleanup.json" || return 1
    delete_if_exists storage rm "${state_uri}/metadata.json" || return 1
  fi
  if [[ "${CLEANUP_DRY_RUN}" != "true" ]]; then rm -rf -- "${dir}"; fi
}

dirs=()
plan='[]'
case "${1:-}" in
  service-account)
    : "${GCP_PROJECT_ID:?GCP_PROJECT_ID is required}"
    email="${2:?email is required}"; expected_id="${3:?ID is required}"; infra="${4:?infra ID is required}"
    [[ "${infra}" =~ ^ocp-[a-f0-9]{10}-[a-z0-9]{5}$ && "${email}" == *@"${GCP_PROJECT_ID}.iam.gserviceaccount.com" ]] || exit 2
    account="$(gcloud iam service-accounts describe "${email}" --project="${GCP_PROJECT_ID}" --format=json)"
    jq -L "$(dirname "${BASH_SOURCE[0]}")" -e --arg id "${expected_id}" --arg infra "${infra}" '
      include "policy"; .uniqueId == $id and (.displayName | test_infra) == $infra and
      ({name:$infra} | protected | not)' <<< "${account}" >/dev/null || exit 1
    instances="$(gcloud compute instances list --project="${GCP_PROJECT_ID}" --format=json)"
    jq -e --arg email "${email}" 'all(.[]; all(.serviceAccounts[]?; .email != $email))' <<< "${instances}" >/dev/null || {
      echo "SKIP account: still attached to a VM" >&2; exit 1;
    }
    cleanup_header "OpenShift orphan service accounts"
    cleanup_report service-account "$(jq -c '[{name:.email,id:.uniqueId}]' <<< "${account}")"
    cleanup_change gcloud iam service-accounts delete "${email}" --project="${GCP_PROJECT_ID}" --quiet
    cleanup_change gcloud storage rm "${GCP_CLEANUP_STATE_URI}/orphan-accounts/${expected_id}.json"
    exit
    ;;
  install)
    [[ $# -eq 2 ]] || { echo "Usage: $0 install DIR [--dry-run]" >&2; exit 2; }
    dirs+=("$2")
    ;;
  recorded-installs|multicluster)
    [[ $# -eq 1 ]] || exit 2
    : "${GCP_PROJECT_ID:?GCP_PROJECT_ID is required}"
    plan="$(multicluster_plan)"
    if [[ "$1" == "recorded-installs" ]]; then
      while IFS= read -r dir; do
        if [[ -f "${dir}/metadata.json" ]]; then
          dirs+=("${dir}")
        else
          echo "SKIP ${dir}: no installer metadata; keeping it for diagnostics"
        fi
      done < <(install_dirs)
    fi
    ;;
  *) echo "Usage: $0 install DIR | recorded-installs | multicluster [--dry-run]" >&2; exit 2 ;;
esac
cleanup_header "OpenShift ${project_id:-installer metadata}"
cleanup_report multicluster-resource "${plan}"
for dir in "${dirs[@]}"; do report_install "${dir}"; done

status=0
while IFS= read -r resource; do
  type="$(jq -r '.type' <<< "${resource}")"
  name="$(jq -r '.name' <<< "${resource}")"
  if [[ "${type}" == "peerings" ]]; then
    delete_if_exists compute networks peerings delete "${name}" --network="$(jq -r '.network' <<< "${resource}")" --project="${project_id}" --quiet || status=1
  else
    delete_if_exists compute firewall-rules delete "${name}" --project="${project_id}" --quiet || status=1
  fi
done < <(jq -c '.[]' <<< "${plan}")
for dir in "${dirs[@]}"; do destroy_install "${dir}" || status=1; done
exit "${status}"
