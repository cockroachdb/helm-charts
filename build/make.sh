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
# breaking the existing legacy skip logic below.
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

  # Always package v2 charts, including in prod. The release step treats
  # already-published OCI artifacts as success and GCS uploads overwrite the
  # same chart package, so rerunning a partial publish is safe.
  $HELM_INSTALL_DIR/helm package cockroachdb-parent/charts/operator --destination "${v2_artifacts_dir}"
  $HELM_INSTALL_DIR/helm package cockroachdb-parent/charts/cockroachdb --destination "${v2_artifacts_dir}"

  $HELM_INSTALL_DIR/helm repo index "${v2_artifacts_dir}" --url "https://${charts_hostname}/v2" --merge "${v2_artifacts_dir}/old-index.yaml"
  diff -u "${v2_artifacts_dir}/old-index.yaml" "${v2_artifacts_dir}/index.yaml" || true
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
