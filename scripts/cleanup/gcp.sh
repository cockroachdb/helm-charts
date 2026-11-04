#!/usr/bin/env bash
# Clean one test VPC and retain LB references so interrupted cleanup can retry.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
cleanup_parse_args "$@"
set -- "${CLEANUP_ARGS[@]}"
: "${GCP_PROJECT_ID:?GCP_PROJECT_ID is required}"

compute_list() { gcloud compute "$@" list --project="${GCP_PROJECT_ID}" --format=json; }

# External load balancers may omit the VPC on their backend/forwarding rule.
# Follow their instance-group or target-pool references instead of name prefixes.
discover_load_balancers() {
  local groups instances negs
  groups="$(compute_list instance-groups unmanaged)"
  negs="$(compute_list network-endpoint-groups)"
  groups="$(jq -c --argjson negs "${negs}" '. + $negs' <<< "${groups}")"
  groups="$(jq -c --arg link "${link}" '[.[] | select(.network == $link) | .selfLink]' <<< "${groups}")"
  instances="$(compute_list instances)"
  instances="$(jq -c --arg link "${link}" '[.[] | select(any(.networkInterfaces[]?; .network == $link)) | .selfLink]' <<< "${instances}")"
  pools="$(compute_list target-pools)"
  pools="$(jq -c --argjson instances "${instances}" --argjson plan "${plan}" '[.[] | . as $p | select(
    (($plan.pools // []) | index($p.selfLink)) != null or
    ((.instances // [] | length) > 0 and all(.instances[]; . as $i | ($instances | index($i)) != null)))]' <<< "${pools}")"
  backends="$(compute_list backend-services)"
  backends="$(jq -c --arg link "${link}" --argjson groups "${groups}" --argjson plan "${plan}" '[.[] | . as $b | select(
    .network == $link or (($plan.backends // []) | index($b.selfLink)) != null or
    ((.backends // [] | length) > 0 and all(.backends[].group; . as $g | ($groups | index($g)) != null)))]' <<< "${backends}")"
  rules="$(compute_list forwarding-rules)"
  rules="$(jq -c --arg link "${link}" --argjson pools "${pools}" --argjson backends "${backends}" '[.[] | . as $r | select(
    .network == $link or any($pools[]; .selfLink == $r.target) or any($backends[]; .selfLink == $r.backendService))]' <<< "${rules}")"
}

delete_compute() {
  local family="$1" resource="$2" name zone region
  name="$(jq -er '.name' <<< "${resource}")"
  zone="$(jq -r '.zone // empty | split("/")[-1]' <<< "${resource}")"
  region="$(jq -r '.region // empty | split("/")[-1]' <<< "${resource}")"
  local args=()
  read -r -a args <<< "${family}"
  args=(compute "${args[@]}" delete "${name}" --project="${GCP_PROJECT_ID}" --quiet)
  if [[ -n "${zone}" ]]; then
    args+=(--zone="${zone}")
  elif [[ -n "${region}" ]]; then
    args+=(--region="${region}")
  elif [[ "${family}" != "networks" && "${family}" != "firewall-rules" && "${family}" != *"http-health-checks" && "${family}" != "https-health-checks" ]]; then
    args+=(--global)
  fi
  cleanup_change gcloud "${args[@]}"
}

network_resources() {
  local family="$1" resources parts=()
  read -r -a parts <<< "${family}"
  resources="$(compute_list "${parts[@]}")" || return 1
  jq -c --arg family "${family}" --arg link "${link}" --argjson network "${network}" --argjson plan "${plan}" '
    [.[] | . as $r | select(
      if ($family | endswith("health-checks")) then ($plan.health_checks | index($r.selfLink)) != null
      elif $family == "addresses" then (($network.subnetworks // []) | index($r.subnetwork)) != null or ($plan.addresses | index($r.selfLink)) != null
      elif $family == "instance-groups unmanaged" then .network == $link or ($plan.groups | index($r.selfLink)) != null
      else .network == $link end)]' <<< "${resources}"
}

cleanup_network() (
  local network_name="$1" expected_id="${2:-}" network link clusters rules backends pools addresses state_uri plan_uri plan output row family
  [[ "${network_name}" =~ ^cockroachdb-vpc-[a-z0-9]+$ ]] || { echo "Invalid test VPC name" >&2; return 1; }
  if ! network="$(gcloud compute networks describe "${network_name}" --project="${GCP_PROJECT_ID}" --format=json 2>&1)"; then
    cleanup_missing "${network}" && return 0
    echo "${network}" >&2; return 1
  fi
  if [[ -n "${expected_id}" ]] && [[ "$(jq -r '.id' <<< "${network}")" != "${expected_id}" ]]; then
    echo "VPC ${network_name} was replaced; review its new ID" >&2; return 1
  fi
  link="$(jq -er '.selfLink' <<< "${network}")"
  clusters="$(gcloud container clusters list --project="${GCP_PROJECT_ID}" --format=json)"
  clusters="$(jq -c --arg network "${network_name}" '[.[] | select((.network | split("/")[-1]) == $network)]' <<< "${clusters}")"
  if [[ -n "${GCP_CLEANUP_EXPECTED_CLUSTERS_JSON:-}" ]]; then
    jq -e --argjson expected "${GCP_CLEANUP_EXPECTED_CLUSTERS_JSON}" 'all(.[]; . as $c | $expected[$c.name] == $c.id)' <<< "${clusters}" >/dev/null || {
      echo "VPC ${network_name} contains a cluster outside the inventory" >&2; return 1;
    }
  elif [[ -n "${GCP_CLEANUP_OWNER_RUN_ID:-}" ]]; then
    jq -e --arg run "${GCP_CLEANUP_OWNER_RUN_ID}" 'all(.[]; .resourceLabels["managed-by"] == "helm-charts-e2e" and .resourceLabels["github-run-id"] == $run)' <<< "${clusters}" >/dev/null || {
      echo "VPC ${network_name} contains a cluster owned by another run" >&2; return 1;
    }
  fi
  local work
  work="$(mktemp -d)"
  trap 'rm -rf "${work}"' EXIT
  if [[ "${GCP_CLEANUP_REUSE_CONTEXTS:-false}" != "true" ]]; then export KUBECONFIG="${work}/kubeconfig"; fi
  state_uri="${GCP_CLEANUP_STATE_URI:-}"
  if [[ -z "${state_uri}" && "${GITHUB_ACTIONS:-}" == "true" ]]; then
    state_uri="gs://${GCP_PROJECT_ID}-openshift-ci-debug/cleanup"
  fi
  plan='{"health_checks":[],"groups":[]}'
  plan_uri="${state_uri}/gcp-${network_name}/compute-cleanup.json"
  if [[ -n "${state_uri}" ]]; then
    if output="$(gcloud storage cat "${plan_uri}" 2>"${work}/error")"; then
      jq -e --arg link "${link}" --arg id "$(jq -r '.id' <<< "${network}")" '.network == $link and .network_id == $id' <<< "${output}" >/dev/null || { echo "Cleanup plan has a different VPC" >&2; return 1; }
      plan="${output}"
    elif ! cleanup_missing "$(cat "${work}/error")"; then
      cat "${work}/error" >&2; return 1
    fi
  fi
  discover_load_balancers
  addresses="$(compute_list addresses)"
  addresses="$(jq -c --argjson rules "${rules}" '[.[] | . as $a | select(any($rules[]; .IPAddress == $a.address)) | .selfLink]' <<< "${addresses}")"
  plan="$(jq -c --arg link "${link}" --arg id "$(jq -r '.id' <<< "${network}")" --argjson backends "${backends}" --argjson pools "${pools}" '
    .network=$link | .network_id=$id |
    .health_checks=((.health_checks + [$backends[].healthChecks[]?, $pools[].healthChecks[]?]) | unique) |
    .groups=((.groups + [$backends[].backends[]?.group]) | unique) |
    .backends=(((.backends // []) + [$backends[].selfLink]) | unique) |
    .pools=(((.pools // []) + [$pools[].selfLink]) | unique)' <<< "${plan}")"
  plan="$(jq -c --argjson addresses "${addresses}" '.addresses=(((.addresses // []) + $addresses) | unique)' <<< "${plan}")"
  cleanup_header "GCP project ${GCP_PROJECT_ID}, VPC ${network_name}"
  cleanup_report network "$(jq -c '[.]' <<< "${network}")"
  cleanup_report gke-cluster "${clusters}"
  cleanup_report forwarding-rules "${rules}"
  cleanup_report backend-services "${backends}"
  cleanup_report target-pools "${pools}"
  local families=(health-checks http-health-checks https-health-checks network-endpoint-groups 'instance-groups unmanaged' firewall-rules addresses 'networks subnets')
  local resources
  for family in "${families[@]}"; do
    resources="$(network_resources "${family}")"
    cleanup_report "${family}" "${resources}"
  done
  while IFS= read -r row; do
    local name location services context_name kube_args=()
    name="$(jq -r '.name' <<< "${row}")"; location="$(jq -r '.location' <<< "${row}")"
    context_name="${name}"
    if [[ "${GCP_CLEANUP_REUSE_CONTEXTS:-false}" != "true" ]]; then
      gcloud container clusters get-credentials "${name}" --location="${location}" --project="${GCP_PROJECT_ID}" || true
      context_name="gke_${GCP_PROJECT_ID}_${location}_${name}"
    fi
    kube_args=(--context="${context_name}" --request-timeout=30s)
    if services="$(kubectl "${kube_args[@]}" get services --all-namespaces -o json)"; then
      services="$(jq -c '[.items[] | select(.spec.type == "LoadBalancer")]' <<< "${services}")"
    else
      echo "Cluster API unavailable; cloud-side cleanup will handle load balancers" >&2
      services='[]'
    fi
    printf '%s\n' "${services}" > "${work}/${name}-services.json"
    cleanup_report loadbalancer-service "$(jq -c --arg cluster "${name}" '[.[] | {name:(.metadata.namespace + "/" + .metadata.name), location:$cluster}]' <<< "${services}")"
  done < <(jq -c '.[]' <<< "${clusters}")

  printf '%s\n' "${plan}" > "${work}/compute-cleanup.json"
  if [[ -n "${state_uri}" ]]; then
    cleanup_change gcloud storage cp "${work}/compute-cleanup.json" "${plan_uri}"
  fi
  while IFS= read -r row; do
    name="$(jq -r '.name' <<< "${row}")"; location="$(jq -r '.location' <<< "${row}")"
    context_name="${name}"
    if [[ "${GCP_CLEANUP_REUSE_CONTEXTS:-false}" != "true" ]]; then context_name="gke_${GCP_PROJECT_ID}_${location}_${name}"; fi
    kube_args=(--context="${context_name}" --request-timeout=30s)
    while IFS= read -r service; do
      cleanup_change kubectl "${kube_args[@]}" delete service "$(jq -r '.metadata.name' <<< "${service}")" \
        --namespace="$(jq -r '.metadata.namespace' <<< "${service}")" --ignore-not-found --wait=true --timeout=5m || \
        echo "Service cleanup failed; continuing cloud-side LB cleanup" >&2
    done < <(jq -c '.[]' "${work}/${name}-services.json")
    cleanup_change gcloud container clusters delete "${name}" --location="${location}" --project="${GCP_PROJECT_ID}" --quiet
  done < <(jq -c '.[]' <<< "${clusters}")

  # GKE may have already removed part of the LB graph. Re-list before deleting.
  discover_load_balancers
  while IFS= read -r row; do delete_compute forwarding-rules "${row}"; done < <(jq -c '.[]' <<< "${rules}")
  while IFS= read -r row; do delete_compute backend-services "${row}"; done < <(jq -c '.[]' <<< "${backends}")
  while IFS= read -r row; do delete_compute target-pools "${row}"; done < <(jq -c '.[]' <<< "${pools}")
  for family in "${families[@]}"; do
    resources="$(network_resources "${family}")"
    while IFS= read -r row; do delete_compute "${family}" "${row}"; done < <(jq -c '.[]' <<< "${resources}")
  done
  delete_compute networks "${network}"
  if [[ -n "${state_uri}" ]]; then cleanup_change gcloud storage rm "${plan_uri}"; fi
)

# Recover dependencies whose cluster/VPC disappeared before teardown completed.
cleanup_leftovers() (
  local cutoff="$1" work family resources row clusters groups instances networks backends pools rules
  [[ "${cutoff}" =~ ^[0-9]+$ ]] || return 2
  work="$(mktemp -d)"
  trap 'rm -rf "${work}"' EXIT
  clusters="$(gcloud container clusters list --project="${GCP_PROJECT_ID}" --format=json)"
  groups="$(compute_list instance-groups unmanaged)"
  groups="$(jq -c --argjson negs "$(compute_list network-endpoint-groups)" '. + $negs | map(.selfLink)' <<< "${groups}")"
  instances="$(compute_list instances)"
  networks="$(compute_list networks)"
  backends="$(compute_list backend-services)"
  pools="$(compute_list target-pools)"
  rules="$(compute_list forwarding-rules)"
  # A dangling DNS load balancer has no surviving network, instance or backend group.
  backends="$(jq -L "$(dirname "${BASH_SOURCE[0]}")" -c --argjson groups "${groups}" --argjson networks "${networks}" --argjson cutoff "${cutoff}" '
    include "policy"; [.[] | . as $b |
    select((try (.description | fromjson | (."kubernetes.io/service-name" // ."networking.gke.io/service-name")) catch "") == "kube-system/crl-core-dns") |
    select((protected | not) and (.creationTimestamp | old($cutoff))) |
    select(all($networks[]; .selfLink != $b.network)) |
    select(all(.backends[]?; .group as $g | ($groups | index($g)) == null))]' <<< "${backends}")"
  pools="$(jq -L "$(dirname "${BASH_SOURCE[0]}")" -c --argjson instances "${instances}" --argjson cutoff "${cutoff}" '
    include "policy"; [.[] |
    select((try (.description | fromjson | ."kubernetes.io/service-name") catch "") == "kube-system/crl-core-dns") |
    select((protected | not) and (.creationTimestamp | old($cutoff))) |
    select(all(.instances[]?; . as $i | all($instances[]; .selfLink != $i)))]' <<< "${pools}")"
  # Preserve the whole backend when any forwarding rule using it is protected.
  backends="$(jq -L "$(dirname "${BASH_SOURCE[0]}")" -c --argjson rules "${rules}" 'include "policy"; [.[] | .selfLink as $b | select(all($rules[]; .backendService != $b or (protected | not)))]' <<< "${backends}")"
  pools="$(jq -L "$(dirname "${BASH_SOURCE[0]}")" -c --argjson rules "${rules}" 'include "policy"; [.[] | .selfLink as $p | select(all($rules[]; .target != $p or (protected | not)))]' <<< "${pools}")"
  rules="$(jq -c --argjson backends "${backends}" --argjson pools "${pools}" '[.[] | . as $r | select(any($backends[]; .selfLink == $r.backendService) or any($pools[]; .selfLink == $r.target))]' <<< "${rules}")"
  cleanup_header "GCP detached test dependencies"
  cleanup_report forwarding-rules "${rules}"
  cleanup_report backend-services "${backends}"
  cleanup_report target-pools "${pools}"
  while IFS= read -r row; do delete_compute forwarding-rules "${row}"; done < <(jq -c '.[]' <<< "${rules}")
  while IFS= read -r row; do delete_compute backend-services "${row}"; done < <(jq -c '.[]' <<< "${backends}")
  while IFS= read -r row; do delete_compute target-pools "${row}"; done < <(jq -c '.[]' <<< "${pools}")
  # Keep all checks referenced by any surviving load balancer, including OpenShift.
  backends="$(compute_list backend-services)"
  pools="$(compute_list target-pools)"
  for family in health-checks http-health-checks; do
    resources="$(compute_list "${family}")"
    resources="$(jq -L "$(dirname "${BASH_SOURCE[0]}")" -c --argjson backends "${backends}" --argjson pools "${pools}" --argjson cutoff "${cutoff}" '
      include "policy"; [.[] | . as $h |
      select((.name | test("^(k8s-|k8s2-|a[a-f0-9]{32}$)")) or (.name | test_infra) != null) |
      select((protected | not) and (.creationTimestamp | old($cutoff))) |
      select(all($backends[], $pools[]; all(.healthChecks[]?; . != $h.selfLink)))]' <<< "${resources}")"
    # In dry-run mode selected backends still exist; their checks remain visible
    # in the normal VPC plan and will be recovered on the following sweep.
    cleanup_report "${family}" "${resources}"
    while IFS= read -r row; do delete_compute "${family}" "${row}"; done < <(jq -c '.[]' <<< "${resources}")
  done
  resources="$(compute_list disks)"
  resources="$(jq -L "$(dirname "${BASH_SOURCE[0]}")" -c --argjson clusters "${clusters}" --argjson cutoff "${cutoff}" '
    include "policy"; [.[] | .labels["goog-k8s-cluster-name"] as $cluster |
    select(($cluster // "" | test_cluster) and ((.users // []) | length == 0)) |
    select((protected | not) and ({name:$cluster} | protected | not) and (.creationTimestamp | old($cutoff))) |
    select(all($clusters[]; .name != $cluster))]' <<< "${resources}")"
  cleanup_report persistent-disk "${resources}"
  while IFS= read -r row; do delete_compute disks "${row}"; done < <(jq -c '.[]' <<< "${resources}")
)

case "${1:-}" in
  leftovers) cleanup_leftovers "${2:?age cutoff is required}" ;;
  network) cleanup_network "${2:?network is required}" "${3:-}" ;;
  *)
    echo "Usage: $0 network NAME [EXPECTED_ID] | leftovers CUTOFF [--dry-run]" >&2
    exit 2
    ;;
esac
