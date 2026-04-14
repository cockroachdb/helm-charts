#!/usr/bin/env bash

set -euxo pipefail

charts_hostname="${CHARTS_HOSTNAME:-charts.cockroachdb.com}"
case $charts_hostname in
  charts.cockroachdb.com)
    gcs_bucket=cockroach-helm-charts-prod
    is_prod=true
    ;;
  charts-test.cockroachdb.com)
    gcs_bucket=cockroach-helm-charts-test
    is_prod=false
    ;;
  *)
    echo "unknown host $charts_hostname"
    exit 1
    ;;
esac

artifacts_dir="build/artifacts/"
v2_artifacts_dir="build/artifacts/v2/"
HELM_INSTALL_DIR=$PWD/bin

install_helm() {
  mkdir -p "$HELM_INSTALL_DIR"
  curl -fsSL -o "$HELM_INSTALL_DIR/get_helm.sh" https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3
  chmod 700 "$HELM_INSTALL_DIR/get_helm.sh"
  export PATH="$HELM_INSTALL_DIR":$PATH
  HELM_INSTALL_DIR="$HELM_INSTALL_DIR" "$HELM_INSTALL_DIR/get_helm.sh" --no-sudo --version v3.13.3
}

# chart_version_exists returns 1 (failure) when the version exists, which is
# inverted from standard convention. This legacy behavior is preserved to avoid
# breaking the existing skip logic below. See v2_chart_version_exists for the
# standard convention (returns 0 when exists).
chart_version_exists() {
  helm repo add cockroachdb "https://${charts_hostname}" --force-update
  helm repo update

  existing_version=$(bin/yq '.version' cockroachdb/Chart.yaml)
  if helm search repo cockroachdb/cockroachdb --version "$existing_version" | grep $existing_version; then
    echo "Chart version $existing_version already exists in the repository."
    return 1
  fi
  return 0
}

build_chart() {
  mkdir -p "$artifacts_dir"
  # Grab the current index.yaml to merge into the new index.yaml.
  curl -fsSL "https://storage.googleapis.com/$gcs_bucket/index.yaml" > "${artifacts_dir}/old-index.yaml"

  # Build the charts
  $HELM_INSTALL_DIR/helm package cockroachdb --destination "${artifacts_dir}"
  $HELM_INSTALL_DIR/helm repo index "${artifacts_dir}" --url "https://${charts_hostname}" --merge "${artifacts_dir}/old-index.yaml"
  diff -u "${artifacts_dir}/old-index.yaml" "${artifacts_dir}/index.yaml" || true
}

# v2_chart_version_exists checks if a specific chart version already exists
# in the v2 Helm repository.
v2_chart_version_exists() {
  local chart_name="$1"
  local chart_dir="$2"

  helm repo add cockroachdb-v2 "https://${charts_hostname}/v2" --force-update 2>/dev/null || true
  helm repo update cockroachdb-v2 2>/dev/null || true

  local existing_version
  existing_version=$(bin/yq '.version' "${chart_dir}/Chart.yaml")
  # Use --devel to also match prerelease versions (e.g., 26.1.2-preview).
  if helm search repo "cockroachdb-v2/${chart_name}" --devel --version "$existing_version" 2>/dev/null | grep -q "$existing_version"; then
    echo "Chart ${chart_name} version $existing_version already exists in v2 repository."
    return 0
  fi
  return 1
}

# build_v2_charts packages the operator and cockroachdb charts from
# cockroachdb-parent/charts/ into build/artifacts/v2/ and generates
# a merged v2 index.yaml for the Helm repository.
build_v2_charts() {
  mkdir -p "$v2_artifacts_dir"

  # Fetch the current v2 index.yaml to merge into. If v2 has never been
  # published, start with an empty index.
  if curl -fsSL "https://storage.googleapis.com/$gcs_bucket/v2/index.yaml" > "${v2_artifacts_dir}/old-index.yaml" 2>/dev/null; then
    echo "Fetched existing v2 index.yaml"
  else
    echo "No existing v2 index.yaml found, starting fresh"
    echo -e "apiVersion: v1\nentries: {}" > "${v2_artifacts_dir}/old-index.yaml"
  fi

  local packaged=false

  # Package operator chart (skip if version already published in prod).
  if [ "$is_prod" = true ] && v2_chart_version_exists "operator" "cockroachdb-parent/charts/operator"; then
    echo "Skipping operator chart: version already published."
  else
    $HELM_INSTALL_DIR/helm package cockroachdb-parent/charts/operator --destination "${v2_artifacts_dir}"
    packaged=true
  fi

  # Package cockroachdb chart (skip if version already published in prod).
  if [ "$is_prod" = true ] && v2_chart_version_exists "cockroachdb" "cockroachdb-parent/charts/cockroachdb"; then
    echo "Skipping cockroachdb chart: version already published."
  else
    $HELM_INSTALL_DIR/helm package cockroachdb-parent/charts/cockroachdb --destination "${v2_artifacts_dir}"
    packaged=true
  fi

  if [ "$packaged" = true ]; then
    $HELM_INSTALL_DIR/helm repo index "${v2_artifacts_dir}" --url "https://${charts_hostname}/v2" --merge "${v2_artifacts_dir}/old-index.yaml"
    diff -u "${v2_artifacts_dir}/old-index.yaml" "${v2_artifacts_dir}/index.yaml" || true
  else
    echo "No new v2 charts to package."
  fi
}

# When invoked directly (not sourced), run legacy chart build.
# Use build_v2_charts for v2 chart packaging.
if [[ "${1:-}" == "v2" ]]; then
  install_helm
  build_v2_charts
else
  install_helm
  if [ "$is_prod" = true ] && ! chart_version_exists; then
    echo "Skipping build: chart version already present in production."
    exit 0
  fi
  build_chart
fi
