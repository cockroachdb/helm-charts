#!/usr/bin/env bash
# Discovery helpers sourced by sweep.sh; mutations run only after discovery.

cluster_eligible() {
  local cluster="$1" metadata
  policy -e 'include "policy"; protected' <<< "${cluster}" >/dev/null && return 1
  if jq -e '.resourceLabels["managed-by"] != null' <<< "${cluster}" >/dev/null; then
    metadata="$(jq -c --arg repo "${repository}" --arg project "${GCP_PROJECT_ID}" '{managed_by:.resourceLabels["managed-by"],repository:$repo,project:$project,provider:"gcp",run_id:.resourceLabels["github-run-id"],created_at:.resourceLabels["created-at"]}' <<< "${cluster}")"
    eligible "${metadata}" gcp "${GCP_PROJECT_ID}"
  else
    policy -e --argjson cutoff "${cutoff}" 'include "policy"; (.name | test_cluster) and (.createTime | old($cutoff))' <<< "${cluster}" >/dev/null && repository_quiet
  fi
}

discover_networks() {
  local network name metadata cluster members allowed
  while IFS= read -r network; do
    name="$(jq -r '.name' <<< "${network}")"
    policy -e 'include "policy"; protected' <<< "${network}" >/dev/null && { echo "SKIP ${name}: protected"; continue; }
    metadata="$(jq -c 'try (.description | fromjson) catch {}' <<< "${network}")"
    if jq -e 'has("managed_by")' <<< "${metadata}" >/dev/null; then
      eligible "${metadata}" gcp "${GCP_PROJECT_ID}" || continue
    else
      [[ "${name}" =~ ^cockroachdb-vpc-[a-z0-9]{6}$ ]] || continue
      policy -e --argjson cutoff "${cutoff}" 'include "policy"; .creationTimestamp | old($cutoff)' <<< "${network}" >/dev/null || continue
      repository_quiet || continue
    fi
    members="$(jq -c --arg name "${name}" '[.[] | select((.network | split("/")[-1]) == $name)]' "${work}/clusters.json")"
    allowed=true
    while IFS= read -r cluster; do
      if ! cluster_eligible "${cluster}"; then
        echo "SKIP ${name}: contains an active, young, protected or unrecognized cluster"
        allowed=false; break
      fi
    done < <(jq -c '.[]' <<< "${members}")
    [[ "${allowed}" == true ]] || continue
    jq -c --argjson clusters "$(jq -c 'map({key:.name,value:.id}) | from_entries' <<< "${members}")" '{type:"network",name:.name,id:.id,clusters:$clusters}' <<< "${network}" >> "${work}/selected.jsonl"
  done < <(jq -c '.[] | select(.name | startswith("cockroachdb-vpc-"))' "${work}/networks.json")
}

list_saved_metadata() {
  local pattern="$1" output="$2"
  if ! gcloud storage ls --json "${pattern}" > "${output}" 2> "${work}/error"; then
    cleanup_missing "$(cat "${work}/error")" || { cat "${work}/error" >&2; exit 1; }
    echo '[]' > "${output}"
  fi
}

discover_openshift() {
  local family row infra metadata uri owner run created region records
  touch "${work}/known-infra" "${work}/evidence.jsonl"
  # Include global leftovers and IAM accounts even when the VPC is already gone.
  for family in instances disks health-checks http-health-checks forwarding-rules addresses; do
    gcloud compute "${family}" list --project="${GCP_PROJECT_ID}" --format=json > "${work}/${family}.json"
  done
  gcloud iam service-accounts list --project="${GCP_PROJECT_ID}" --format=json > "${work}/accounts.json"
  gcloud storage buckets list --project="${GCP_PROJECT_ID}" --format=json > "${work}/buckets.json"
  policy -c 'include "policy"; .[] | (.name | test_infra) as $infra | select($infra != null) |
    {infra:$infra,created_at:((.creationTimestamp // .creation_time // .timeCreated) | epoch),
     region:((.region // (.zone // "" | sub("-[a-z]$"; "")) // "") | split("/")[-1]),protected:protected}' \
    "${work}/networks.json" "${work}/instances.json" "${work}/disks.json" "${work}/health-checks.json" \
    "${work}/http-health-checks.json" "${work}/forwarding-rules.json" "${work}/addresses.json" >> "${work}/evidence.jsonl"
  policy -c 'include "policy"; .[] | (.name | test_infra) as $infra | select($infra != null) |
    {infra:$infra,created_at:((.creation_time // .timeCreated) | epoch),region:(.location | ascii_downcase),protected:protected}' "${work}/buckets.json" >> "${work}/evidence.jsonl"
  policy -c 'include "policy"; .[] | (.displayName // "" | test_infra) as $infra | select($infra != null) |
    {infra:$infra,created_at:0,region:"",protected:false}' "${work}/accounts.json" >> "${work}/evidence.jsonl"

  list_saved_metadata "${GCP_CLEANUP_STATE_URI}/*/ci-cleanup.json" "${work}/owners.json"
  while IFS= read -r row; do
    uri="$(jq -r '.url | split("#")[0]' <<< "${row}")"
    owner="$(gcloud storage cat "${uri}")"
    infra="$(jq -er '.infra_id' <<< "${owner}")"
    [[ "${infra}" =~ ^ocp-[a-f0-9]{10}-[a-z0-9]{5}$ && "${uri}" == "${GCP_CLEANUP_STATE_URI}/${infra}/ci-cleanup.json" ]] || exit 1
    printf '%s\n' "${infra}" >> "${work}/known-infra"
    if jq -se --arg infra "${infra}" 'any(.[]; .infra == $infra and .protected)' "${work}/evidence.jsonl" >/dev/null; then
      echo "SKIP ${infra}: a cluster resource is protected"; continue
    fi
    if eligible "${owner}" openshift "${GCP_PROJECT_ID}" && ! is_protected "${infra}"; then
      prepare_openshift "${infra}" "$(gcloud storage cat "${uri%/*}/metadata.json")"
      printf '%s\n%s\n' "${uri}" "${uri%/*}/metadata.json" >> "${work}/retire-state"
    fi
  done < <(jq -c '.[]' "${work}/owners.json")

  repository_quiet || return 0
  # Existing debug uploads contain installer metadata and a GitHub run directory.
  list_saved_metadata "${GCP_CLEANUP_STATE_URI%/cleanup}/**/*metadata.json" "${work}/saved.json"
  while IFS= read -r row; do
    uri="$(jq -r '.url | split("#")[0]' <<< "${row}")"
    [[ "${uri}" == "${GCP_CLEANUP_STATE_URI%/cleanup}/"* ]] || exit 1
    run="${uri#"${GCP_CLEANUP_STATE_URI%/cleanup}/"}"; run="${run%%/*}"
    if [[ "${uri}" == "${GCP_CLEANUP_STATE_URI}/recovered-installs/"*"/metadata.json" ]]; then
      run=""
    elif [[ "${run}" =~ ^[0-9]+$ ]]; then
      : # Old debug metadata is covered by the repository quiet check.
    else
      continue
    fi
    policy -e --argjson cutoff "${cutoff}" 'include "policy"; .metadata.timeCreated | old($cutoff)' <<< "${row}" >/dev/null || continue
    metadata="$(gcloud storage cat "${uri}")"
    infra="$(jq -er '.infraID' <<< "${metadata}")"
    [[ "${infra}" =~ ^ocp-[a-f0-9]{10}-[a-z0-9]{5}$ ]] || exit 1
    # A current ownership marker takes precedence over older debug uploads.
    if grep -Fxq -- "${infra}" "${work}/known-infra"; then continue; fi
    if [[ -n "${run}" ]]; then
      jq -se --arg infra "${infra}" 'any(.[]; .infra == $infra)' "${work}/evidence.jsonl" >/dev/null || continue
    elif [[ "${uri}" != "${GCP_CLEANUP_STATE_URI}/recovered-installs/${infra}/metadata.json" ]]; then
      exit 1
    fi
    created="$(policy -r 'include "policy"; .metadata.timeCreated | epoch' <<< "${row}")"
    region="$(jq -er '.gcp.region' <<< "${metadata}")"
    jq -cn --arg infra "${infra}" --argjson created "${created}" --arg region "${region}" '{infra:$infra,created_at:$created,region:$region,protected:false}' >> "${work}/evidence.jsonl"
    printf '%s\n' "${metadata}" > "${work}/${infra}-metadata.json"
  done < <(jq -c '.[]' "${work}/saved.json")

  records="$(jq -sc 'group_by(.infra) | map({infra:.[0].infra,created_at:(map(.created_at) | max),
    regions:(map(.region | select(test("^[a-z]+-[a-z]+[0-9]+$"))) | unique),protected:any(.[]; .protected)})' "${work}/evidence.jsonl")"
  while IFS= read -r row; do
    infra="$(jq -r '.infra' <<< "${row}")"
    if grep -Fxq -- "${infra}" "${work}/known-infra"; then continue; fi
    if is_protected "${infra}" || jq -e '.protected' <<< "${row}" >/dev/null; then
      echo "SKIP ${infra}: protected"; continue
    fi
    created="$(jq -r '.created_at' <<< "${row}")"
    if [[ "${created}" -gt "${cutoff}" ]]; then continue; fi
    if [[ "${created}" -gt 0 && "$(jq '.regions | length' <<< "${row}")" -eq 1 ]]; then
      region="$(jq -r '.regions[0]' <<< "${row}")"
      if [[ -f "${work}/${infra}-metadata.json" ]]; then
        metadata="$(cat "${work}/${infra}-metadata.json")"
      else
        metadata="$(jq -cn --arg infra "${infra}" --arg project "${GCP_PROJECT_ID}" --arg region "${region}" '{infraID:$infra,clusterName:($infra | split("-")[:2] | join("-")),gcp:{projectID:$project,region:$region}}')"
      fi
      prepare_openshift "${infra}" "${metadata}"
    else
      echo "SKIP installer cleanup for ${infra}: no unambiguous dated region evidence"
      observe_orphan_accounts "${infra}"
    fi
  done < <(jq -c '.[]' <<< "${records}")
}

observe_orphan_accounts() {
  local infra="$1" account id uri observation
  # Without installer state, only isolated accounts can be recovered this way.
  if policy -se --arg infra "${infra}" 'include "policy"; any(.[][]; (.name | test_infra) == $infra)' \
    "${work}/instances.json" "${work}/disks.json" "${work}/networks.json" >/dev/null; then return 0; fi
  while IFS= read -r account; do
    id="$(jq -er '.uniqueId' <<< "${account}")"
    uri="${GCP_CLEANUP_STATE_URI}/orphan-accounts/${id}.json"
    if observation="$(gcloud storage cat "${uri}" 2> "${work}/error")"; then
      if policy -e --arg id "${id}" --arg infra "${infra}" --argjson cutoff "${cutoff}" 'include "policy"; .id == $id and .infra == $infra and (.created_at | old($cutoff))' <<< "${observation}" >/dev/null; then
        jq -c --arg infra "${infra}" '{type:"service-account",name:.email,id:.uniqueId,infra:$infra}' <<< "${account}" >> "${work}/selected.jsonl"
      fi
    elif cleanup_missing "$(cat "${work}/error")"; then
      jq -cn --arg id "${id}" --arg infra "${infra}" --arg uri "${uri}" --argjson created "$(date +%s)" '{type:"observation",name:$id,id:$id,infra:$infra,uri:$uri,created_at:$created}' >> "${work}/selected.jsonl"
      echo "DEFER ${infra} account: first discovery; requires ${max_age_hours} hours of observation"
    else
      cat "${work}/error" >&2; exit 1
    fi
  done < <(policy -c --arg infra "${infra}" 'include "policy"; .[] | select((.displayName // "" | test_infra) == $infra)' "${work}/accounts.json")
}

discover_gcp() {
  gcloud storage buckets describe "${GCP_CLEANUP_STATE_URI%/cleanup}" >/dev/null
  gcloud compute networks list --project="${GCP_PROJECT_ID}" --format=json > "${work}/networks.json"
  gcloud container clusters list --project="${GCP_PROJECT_ID}" --format=json > "${work}/clusters.json"
  discover_networks
  discover_openshift
}
