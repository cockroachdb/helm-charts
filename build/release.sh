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
    is_prod=true
    ;;
  charts-test.cockroachdb.com)
    lb_name=cockroach-helm-charts-test-default
    gcs_bucket=cockroach-helm-charts-test
    google_credentials="$GCS_CREDENTIALS_PROD"
    google_project=releases-prod
    is_prod=false
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

  local release_failed=false

  # Publish destinations independently so a registry outage does not prevent
  # publishing to the remaining destinations. DockerHub is best-effort while
  # the repos are being provisioned; GAR and GCS still determine release status.
  if [ "$is_prod" = true ]; then
    push_oci_dockerhub
    if ! push_oci_gar; then
      echo "GAR OCI publish failed."
      release_failed=true
    fi
  else
    echo "Skipping OCI pushes for ${charts_hostname}; publishing only the test Helm repo."
  fi

  if ! publish_gcs_v2; then
    echo "GCS v2 publish failed."
    release_failed=true
  fi

  if [ "$release_failed" = true ]; then
    return 1
  fi
}

publish_gcs_v2() {
  local gcs_changed=false
  local gcs_failed=false

  # Upload v2 chart packages and index.yaml individually to GCS. Existing
  # identical chart packages are skipped; same-name packages with different
  # content fail so reruns are idempotent but version reuse is blocked.
  for tgz in build/artifacts/v2/*.tgz; do
    if ! publish_gcs_chart "$tgz"; then
      gcs_failed=true
    fi
  done

  if [ "$gcs_failed" = true ]; then
    echo "Skipping v2 index upload because one or more chart packages failed to publish."
    return 1
  fi

  if [ "$gcs_changed" = true ] || ! gcs_index_exists; then
    if ! gsutil cp "build/artifacts/v2/index.yaml" "gs://${gcs_bucket}/v2/index.yaml"; then
      return 1
    fi

    # Invalidate cached v2 index.yaml last so all destinations are populated first.
    if ! gcloud --project "$google_project" compute url-maps invalidate-cdn-cache "$lb_name" --path "/v2/index.yaml" --async; then
      return 1
    fi
  else
    echo "No GCS chart changes detected; skipping v2 index upload and CDN invalidation."
  fi
}

# push_oci_gar pushes chart packages as OCI artifacts to Google Artifact Registry.
# Uses the gcloud service account (already activated by gcs_authenticate) to obtain
# an access token for Helm OCI registry login.
push_oci_gar() {
  local gar_registry="${OCI_GAR_REGISTRY:-us-docker.pkg.dev/releases-prod/self-hosted/charts}"
  local gar_host="${gar_registry%%/*}"
  local failed=false

  echo "Authenticating with GAR for OCI push (${gar_host})..."
  if ! gcloud auth print-access-token | "$HELM" registry login "${gar_host}" --username oauth2accesstoken --password-stdin; then
    echo "GAR OCI registry login failed."
    return 1
  fi

  echo "Pushing charts to OCI registry: ${gar_registry}"
  for chart_pkg in build/artifacts/v2/*.tgz; do
    echo "  Pushing ${chart_pkg}..."
    if ! push_with_retry "${chart_pkg}" "${gar_registry}"; then
      failed=true
    fi
  done

  if [ "$failed" = true ]; then
    return 1
  fi
}

publish_gcs_chart() {
  local chart_pkg="$1"
  local object_uri="gs://${gcs_bucket}/v2/$(basename "$chart_pkg")"
  local exists_status

  exists_status=0
  gcs_chart_exists "$chart_pkg" "$object_uri" || exists_status=$?

  if [ "$exists_status" -eq 0 ]; then
    echo "  Chart already exists at ${object_uri}; skipping."
    return 0
  fi

  if [ "$exists_status" -eq 2 ]; then
    return 1
  fi

  if ! gsutil cp "$chart_pkg" "gs://${gcs_bucket}/v2/"; then
    return 1
  fi
  gcs_changed=true
}

gcs_index_exists() {
  gsutil -q stat "gs://${gcs_bucket}/v2/index.yaml"
}

gcs_chart_exists() {
  local chart_pkg="$1" object_uri="$2"
  local remote_hash local_hash

  if ! remote_hash="$(gsutil hash -h "$object_uri" 2>/dev/null | awk '/Hash [(]sha256[)]:/ {print $3}')"; then
    return 1
  fi

  local_hash="$(openssl dgst -sha256 -binary "$chart_pkg" | openssl base64)"
  if [ "$remote_hash" = "$local_hash" ]; then
    return 0
  fi

  echo "Chart ${object_uri} already exists with different content. Bump the chart version before publishing changed content."
  return 2
}

# push_oci_dockerhub pushes chart packages as OCI artifacts to DockerHub.
# This is best-effort so missing DockerHub repos do not block GAR/GCS publishing.
push_oci_dockerhub() {
  local dockerhub_registry="${OCI_DOCKERHUB_REGISTRY:-registry-1.docker.io/cockroachdb-charts}"
  local failed=false

  if [ -z "${DOCKERHUB_USERNAME:-}" ] || [ -z "${DOCKERHUB_TOKEN:-}" ]; then
    echo "Skipping OCI push to DockerHub: DOCKERHUB_USERNAME and DOCKERHUB_TOKEN not set."
    return 0
  fi

  echo "Authenticating with DockerHub for OCI push..."
  if ! echo "${DOCKERHUB_TOKEN}" | "$HELM" registry login registry-1.docker.io --username "${DOCKERHUB_USERNAME}" --password-stdin; then
    echo "Skipping OCI push to DockerHub: registry login failed."
    return 0
  fi

  echo "Pushing charts to OCI registry: ${dockerhub_registry}"
  for chart_pkg in build/artifacts/v2/*.tgz; do
    echo "  Pushing ${chart_pkg}..."
    if ! push_with_retry "${chart_pkg}" "${dockerhub_registry}"; then
      echo "  Warning: DockerHub OCI push failed for ${chart_pkg}; continuing."
      failed=true
    fi
  done

  if [ "$failed" = true ]; then
    echo "One or more DockerHub OCI pushes failed; continuing with GAR/GCS publishing."
  fi
}

# push_with_retry pushes a chart package to an OCI registry with retries.
# Usage: push_with_retry <chart_pkg> <registry> [max_attempts]
push_with_retry() {
  local chart_pkg="$1" registry="$2" max_attempts="${3:-3}"

  if oci_chart_exists "${chart_pkg}" "${registry}"; then
    echo "  Chart already exists in oci://${registry}; skipping."
    return 0
  fi

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

oci_chart_exists() {
  local chart_pkg="$1" registry="$2"
  local metadata chart_name chart_version

  metadata="$("$HELM" show chart "$chart_pkg")"
  chart_name="$(echo "$metadata" | bin/yq '.name' -)"
  chart_version="$(echo "$metadata" | bin/yq '.version' -)"

  "$HELM" show chart "oci://${registry}/${chart_name}" --version "$chart_version" >/dev/null 2>&1
}

if [[ "${1:-}" == "v2" ]]; then
  release_v2
else
  release_legacy
fi
