#!/usr/bin/env bash

set -euo pipefail

charts_hostname="${CHARTS_HOSTNAME:-charts.cockroachdb.com}"

if [ -n "${DISTRIBUTION_ID-}" ] ; then
  distribution_id="${DISTRIBUTION_ID}"
elif [ "${charts_hostname-}" = "charts.cockroachdb.com" ] ; then
  distribution_id="E2PBFCZT8WAC7B"
elif [ "${charts_hostname-}" = "charts-test.cockroachdb.com" ] ; then
  distribution_id="E20WB6NQP118CN"
fi

# Push the new chart file and updated index.yaml file to S3
# We rely on the aws-cli version installed system-wide.
AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
  aws s3 sync "build/artifacts/" "s3://${charts_hostname}" --exclude old-index.yaml

# Invalidate any cached version of index.yaml (so this version is immediately available)
if [ -n "${distribution_id}" ] ; then
 AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
  aws cloudfront create-invalidation --distribution-id "${distribution_id}" --paths /index.yaml
fi
