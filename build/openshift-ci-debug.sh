#!/usr/bin/env bash
set -euo pipefail

project_id="${GCP_PROJECT_ID:-}"
if [[ -z "${project_id}" ]]; then
  echo "GCP_PROJECT_ID is required" >&2
  exit 1
fi

debug_dir="${OPENSHIFT_DEBUG_DIR:-${RUNNER_TEMP:-/tmp}/openshift-debug}"
mkdir -p "${debug_dir}"
gcs_uri="${OPENSHIFT_DEBUG_GCS_URI:-}"

timestamp() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

run_gcloud() {
  echo "+ gcloud $*"
  gcloud "$@" || true
}

install_dirs() {
  local tmpdir="${TMPDIR:-${RUNNER_TEMP:-/tmp}}"
  find "${tmpdir}" -maxdepth 1 -type d -name "helm-charts-ocp-*" 2>/dev/null | sort || true
}

infra_ids() {
  local install_dir metadata infra_id
  for install_dir in $(install_dirs); do
    metadata="${install_dir}/metadata.json"
    [[ -f "${metadata}" ]] || continue
    infra_id="$(sed -n 's/.*"infraID"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${metadata}" | head -1)"
    [[ -n "${infra_id}" ]] || continue
    printf "%s\n" "${infra_id}"
  done
}

snapshot() {
  local label="${1:-snapshot}"
  local file="${debug_dir}/gcp-${label}-$(date -u +"%Y%m%dT%H%M%SZ").log"

  {
    echo "OpenShift CI debug snapshot: ${label}"
    echo "Timestamp: $(timestamp)"
    echo "Project: ${project_id}"
    echo

    echo "Install directories:"
    install_dirs
    echo

    echo "Infra IDs from metadata:"
    infra_ids
    echo

    echo "OpenShift networks:"
    run_gcloud compute networks list \
      --project="${project_id}" \
      --filter="name~'^ocp-'" \
      --format="table(name,description,creationTimestamp)"
    echo

    echo "OpenShift instances:"
    run_gcloud compute instances list \
      --project="${project_id}" \
      --filter="name~'^ocp-'" \
      --format="table(name,zone,status,machineType.basename(),networkInterfaces[0].network.basename(),creationTimestamp)"
    echo

    echo "OpenShift disks:"
    run_gcloud compute disks list \
      --project="${project_id}" \
      --filter="name~'^ocp-'" \
      --format="table(name,zone,sizeGb,status,users.basename(),creationTimestamp)"
    echo

    echo "OpenShift forwarding rules:"
    run_gcloud compute forwarding-rules list \
      --project="${project_id}" \
      --filter="name~'^ocp-' OR target~'ocp-'" \
      --format="table(name,region,IPAddress,IPProtocol,loadBalancingScheme,target.basename(),network.basename(),creationTimestamp)"
    echo

    echo "OpenShift addresses:"
    run_gcloud compute addresses list \
      --project="${project_id}" \
      --filter="name~'^ocp-' OR users~'ocp-'" \
      --format="table(name,region,address,status,users.basename(),creationTimestamp)"
    echo

    echo "OpenShift firewall rules:"
    run_gcloud compute firewall-rules list \
      --project="${project_id}" \
      --filter="name~'^ocp-' OR name~'^k8s-' OR network~'ocp-'" \
      --format="table(name,network.basename(),sourceRanges.list(),allowed[].map().firewall_rule().list(),targetTags.list(),creationTimestamp)"
    echo

    echo "OpenShift routers:"
    run_gcloud compute routers list \
      --project="${project_id}" \
      --filter="name~'^ocp-' OR network~'ocp-'" \
      --format="table(name,region,network.basename(),creationTimestamp)"
    echo

    echo "OpenShift compute operations:"
    run_gcloud compute operations list \
      --project="${project_id}" \
      --filter="targetLink~'ocp-' OR name~'ocp-'" \
      --sort-by="~startTime" \
      --limit=80 \
      --format="table(name,operationType,status,statusMessage,targetLink.basename(),zone.basename(),region.basename(),startTime,httpErrorStatusCode,httpErrorMessage)"
    echo

    echo "OpenShift DNS records:"
    if [[ -n "${OPENSHIFT_BASE_DOMAIN:-}" ]]; then
      local zone
      zone="$(printf "%s" "${OPENSHIFT_BASE_DOMAIN}" | tr "." "-")"
      run_gcloud dns record-sets list \
        --project="${project_id}" \
        --zone="${zone}" \
        --format="table(name,type,ttl,rrdatas)"
    else
      echo "OPENSHIFT_BASE_DOMAIN is unset; skipping DNS record snapshot"
    fi
    echo
  } 2>&1 | tee "${file}"
  sync_file "${file}"
}

watch() {
  local interval="${1:-120}"
  while true; do
    snapshot "periodic"
    sleep "${interval}"
  done
}

ensure_gcs() {
  [[ -n "${gcs_uri}" ]] || return 0
  local bucket="${gcs_uri#gs://}"
  bucket="${bucket%%/*}"
  [[ -n "${bucket}" && "${bucket}" != "${gcs_uri}" ]] || return 0

  if ! gcloud storage buckets describe "gs://${bucket}" >/dev/null 2>&1; then
    echo "Warning: debug bucket gs://${bucket} is unavailable; GCS sync may fail"
  fi
  return 0
}

sync_file() {
  [[ -n "${gcs_uri}" ]] || return 0
  local file="$1"
  local destination="${2:-$(basename "${file}")}"
  [[ -f "${file}" ]] || return 0
  gcloud storage cp "${file}" "${gcs_uri}/${destination}" >/dev/null 2>&1 || true
}

sync_debug() {
  [[ -n "${gcs_uri}" ]] || return 0
  local file install_dir install_dir_name
  for file in "${debug_dir}"/*.log test_output.log test_output.json; do
    [[ -f "${file}" ]] || continue
    sync_file "${file}"
  done
  for install_dir in $(install_dirs); do
    install_dir_name="$(basename "${install_dir}")"
    for file in "${install_dir}"/.openshift_install.log "${install_dir}"/metadata.json; do
      [[ -f "${file}" ]] || continue
      sync_file "${file}" "${install_dir_name}-$(basename "${file}")"
    done
  done
}

short_infra_id() {
  local infra_id="$1"
  IFS="-" read -r first second _ <<< "${infra_id}"
  if [[ -n "${first}" && -n "${second}" ]]; then
    printf "%s-%s" "${first}" "${second}"
  else
    printf "%s" "${infra_id}"
  fi
}

delete_if_exists() {
  echo "+ gcloud $*"
  gcloud "$@" || true
}

cleanup_multicluster() {
  mapfile -t ids < <(infra_ids)
  if (( ${#ids[@]} == 0 )); then
    echo "No OpenShift infra IDs found; skipping multi-cluster cleanup"
    return 0
  fi

  local infra_id i j a b short_a short_b
  for infra_id in "${ids[@]}"; do
    delete_if_exists compute firewall-rules delete \
      "${infra_id}-submariner-public-ports-ingress" \
      --project="${project_id}" \
      --quiet
    delete_if_exists compute firewall-rules delete \
      "${infra_id}-submariner-internal-ports-ingress" \
      --project="${project_id}" \
      --quiet
  done

  if (( ${#ids[@]} < 2 )); then
    echo "Fewer than two OpenShift infra IDs found; skipping peer cleanup"
    return 0
  fi

  for ((i = 0; i < ${#ids[@]}; i++)); do
    for ((j = i + 1; j < ${#ids[@]}; j++)); do
      a="${ids[$i]}"
      b="${ids[$j]}"
      short_a="$(short_infra_id "${a}")"
      short_b="$(short_infra_id "${b}")"

      delete_if_exists compute firewall-rules delete \
        "${short_a}-allow-peer-${short_b}" \
        --project="${project_id}" \
        --quiet
      delete_if_exists compute firewall-rules delete \
        "${short_b}-allow-peer-${short_a}" \
        --project="${project_id}" \
        --quiet

      delete_if_exists compute networks peerings delete \
        "${short_a}-to-${short_b}" \
        --network="${a}-network" \
        --project="${project_id}" \
        --quiet
      delete_if_exists compute networks peerings delete \
        "${short_b}-to-${short_a}" \
        --network="${b}-network" \
        --project="${project_id}" \
        --quiet
    done
  done
}

case "${1:-}" in
  snapshot)
    snapshot "${2:-snapshot}"
    ;;
  watch)
    watch "${2:-120}"
    ;;
  cleanup-multicluster)
    cleanup_multicluster
    ;;
  ensure-gcs)
    ensure_gcs
    ;;
  sync)
    sync_debug
    ;;
  *)
    echo "usage: $0 {snapshot [label]|watch [seconds]|cleanup-multicluster|ensure-gcs|sync}" >&2
    exit 2
    ;;
esac
