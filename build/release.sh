#!/usr/bin/env bash

set -euo pipefail

HELM="${HELM:-${PWD}/bin/helm}"

charts_hostname="${CHARTS_HOSTNAME:-charts.cockroachdb.com}"
case $charts_hostname in
  charts.cockroachdb.com)
    lb_name=cockroach-helm-charts-prod-default
    gcs_bucket=cockroach-helm-charts-prod
    google_credentials="$GCS_CREDENTIALS_PROD"
    google_project=releases-prod
    ;;
  charts-test.cockroachdb.com)
    lb_name=cockroach-helm-charts-test-default
    gcs_bucket=cockroach-helm-charts-test
    google_credentials="$GCS_CREDENTIALS_PROD"
    google_project=releases-prod
    ;;
  *)
    echo "unknown host $charts_hostname"
    exit 1
    ;;
esac

remove_files_on_exit() {
  rm -f .google-credentials.json
}

trap remove_files_on_exit EXIT

gcs_authenticate() {
  echo "${google_credentials}" > .google-credentials.json
  gcloud auth activate-service-account --key-file=.google-credentials.json
}

# release_legacy publishes the legacy statefulset chart (cockroachdb/) to the
# root of the GCS bucket.
release_legacy() {
  if [ ! -d build/artifacts ]; then
    echo "Directory build/artifacts does not exist. Skipping legacy release."
    return 0
  fi

  gcs_authenticate

  # Push the new chart file and updated index.yaml file to GCS.
  gsutil rsync -x old-index.yaml "build/artifacts/" "gs://${gcs_bucket}/"

  # Invalidate any cached version of index.yaml (so this version is immediately available).
  gcloud --project "$google_project" compute url-maps invalidate-cdn-cache "$lb_name" --path "/index.yaml" --async
}

# release_v2 publishes the operator and cockroachdb charts to the /v2/ path
# in the GCS bucket, and pushes them as OCI artifacts to container registries.
release_v2() {
  if [ ! -d build/artifacts/v2 ]; then
    echo "Directory build/artifacts/v2 does not exist. Skipping v2 release."
    return 0
  fi

  # Check if there are any .tgz chart packages to publish.
  if ! ls build/artifacts/v2/*.tgz 1>/dev/null 2>&1; then
    echo "No v2 chart packages found. Skipping v2 release."
    return 0
  fi

  gcs_authenticate

  # Push v2 chart packages and index.yaml to GCS under /v2/ prefix.
  gsutil rsync -x old-index.yaml "build/artifacts/v2/" "gs://${gcs_bucket}/v2/"

  # Invalidate cached v2 index.yaml.
  gcloud --project "$google_project" compute url-maps invalidate-cdn-cache "$lb_name" --path "/v2/index.yaml" --async

  # Push charts as OCI artifacts to Google Artifact Registry.
  push_oci_gar

  # Push charts as OCI artifacts to DockerHub.
  push_oci_dockerhub
}

# push_oci_gar pushes chart packages as OCI artifacts to Google Artifact Registry.
# Uses the gcloud service account (already activated by gcs_authenticate) to obtain
# an access token for Helm OCI registry login.
push_oci_gar() {
  local gar_registry="${OCI_GAR_REGISTRY:-us-docker.pkg.dev/releases-prod/self-hosted/charts}"
  local gar_host="${gar_registry%%/*}"

  echo "Authenticating with GAR for OCI push (${gar_host})..."
  gcloud auth print-access-token | "$HELM" registry login "${gar_host}" --username oauth2accesstoken --password-stdin

  echo "Pushing charts to OCI registry: ${gar_registry}"
  for chart_pkg in build/artifacts/v2/*.tgz; do
    echo "  Pushing ${chart_pkg}..."
    push_with_retry "${chart_pkg}" "${gar_registry}"
  done
}

# push_oci_dockerhub pushes chart packages as OCI artifacts to DockerHub.
# Requires DOCKERHUB_USERNAME and DOCKERHUB_TOKEN environment variables.
push_oci_dockerhub() {
  local dockerhub_registry="${OCI_DOCKERHUB_REGISTRY:-registry-1.docker.io/cockroachdb}"

  if [ -z "${DOCKERHUB_USERNAME:-}" ] || [ -z "${DOCKERHUB_TOKEN:-}" ]; then
    echo "Skipping OCI push to DockerHub: DOCKERHUB_USERNAME and DOCKERHUB_TOKEN not set."
    return 0
  fi

  echo "Authenticating with DockerHub for OCI push..."
  echo "${DOCKERHUB_TOKEN}" | "$HELM" registry login registry-1.docker.io --username "${DOCKERHUB_USERNAME}" --password-stdin

  echo "Pushing charts to OCI registry: ${dockerhub_registry}"
  for chart_pkg in build/artifacts/v2/*.tgz; do
    echo "  Pushing ${chart_pkg}..."
    push_with_retry "${chart_pkg}" "${dockerhub_registry}"
  done
}

# push_with_retry pushes a chart package to an OCI registry with retries.
# Usage: push_with_retry <chart_pkg> <registry> [max_attempts]
push_with_retry() {
  local chart_pkg="$1" registry="$2" max_attempts="${3:-3}"
  for ((i=1; i<=max_attempts; i++)); do
    if "$HELM" push "$chart_pkg" "oci://$registry"; then
      return 0
    fi
    echo "  Attempt $i/$max_attempts failed for $chart_pkg"
    if ((i < max_attempts)); then
      sleep $((i * 5))
    fi
  done
  echo "  All $max_attempts attempts exhausted for $chart_pkg"
  return 1
}

if [[ "${1:-}" == "v2" ]]; then
  release_v2
else
  release_legacy
fi
