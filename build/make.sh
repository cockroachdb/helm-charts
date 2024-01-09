#!/usr/bin/env bash

set -euxo pipefail

charts_hostname="${CHARTS_HOSTNAME:-charts.cockroachdb.com}"
case $charts_hostname in
  charts.cockroachdb.com)
    gcs_bucket=cockroach-helm-charts-prod
    ;;
  charts-test.cockroachdb.com)
    gcs_bucket=cockroach-helm-charts-test
    ;;
  *)
    echo "uknown host $charts_hostname"
    exit 1
    ;;
esac

HELM_INSTALL_DIR=$PWD/bin
mkdir -p "$HELM_INSTALL_DIR"
curl -fsSL -o "$HELM_INSTALL_DIR/get_helm.sh" https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3
chmod 700 "$HELM_INSTALL_DIR/get_helm.sh"
export PATH="$HELM_INSTALL_DIR":$PATH
HELM_INSTALL_DIR="$HELM_INSTALL_DIR" "$HELM_INSTALL_DIR/get_helm.sh" --no-sudo --version v3.13.3

artifacts_dir="build/artifacts/"
mkdir -p "$artifacts_dir"

# Grab the current index.yaml to merge into the new index.yaml.
curl -fsSL "https://storage.googleapis.com/$gcs_bucket/index.yaml" > "${artifacts_dir}/old-index.yaml"

# Build the charts
$HELM_INSTALL_DIR/helm package cockroachdb --destination "${artifacts_dir}"
$HELM_INSTALL_DIR/helm repo index "${artifacts_dir}" --url "https://${charts_hostname}" --merge "${artifacts_dir}/old-index.yaml"
diff -u "${artifacts_dir}/old-index.yaml" "${artifacts_dir}/index.yaml" || true
