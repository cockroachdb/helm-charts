#!/usr/bin/env bash

set -euxo pipefail

# Absolute path to the toplevel helm-charts directory.
helm_charts_toplevel="$(dirname "$(cd "$(dirname "${0}")"; pwd)")/"
relative_artifacts_dir="build/artifacts/"
artifacts_dir="/charts/${relative_artifacts_dir}"
builder="${helm_charts_toplevel}/build/builder.sh"
charts_hostname="${CHARTS_HOSTNAME:-charts.cockroachdb.com}"

mkdir -p "${helm_charts_toplevel}${relative_artifacts_dir}"

# Grab the current index.yaml to merge into the new index.yaml.
curl "https://s3.amazonaws.com/${charts_hostname}/index.yaml" > "${relative_artifacts_dir}/old-index.yaml"

# Build the charts
"${builder}" helm package cockroachdb --destination "${artifacts_dir}"
"${builder}" helm repo index "${artifacts_dir}" --url "https://${charts_hostname}" --merge "${artifacts_dir}/old-index.yaml"
