#!/usr/bin/env bash

set -euo pipefail

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
    echo "uknown host $charts_hostname"
    exit 1
    ;;
esac

remove_files_on_exit() {
  rm -f .google-credentials.json
}

trap remove_files_on_exit EXIT

echo "${google_credentials}" > .google-credentials.json
gcloud auth activate-service-account --key-file=.google-credentials.json

# Push the new chart file and updated index.yaml file to GCS.
# We rely on the gcloud CLI version installed system-wide.
gsutil rsync -x old-index.yaml "build/artifacts/" "gs://${gcs_bucket}/"

# Invalidate any cached version of index.yaml (so this version is immediately available)
gcloud --project $google_project compute url-maps invalidate-cdn-cache $lb_name --path "/index.yaml" --async
