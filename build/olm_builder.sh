#!/bin/bash

SRC_DIR=$(pwd)
OLM_PATH="${SRC_DIR}"/build/olm-catalog
COCKROACH_CHART="${SRC_DIR}"/cockroachdb
stableCSV="${OLM_PATH}"/bundle/manifests/cockroachdb.clusterserviceversion.yaml
bundleDockerfile="${OLM_PATH}"/bundle.Dockerfile
metaAnnotations="${OLM_PATH}"/bundle/metadata/annotations.yaml

VERSION=${VERSION:-""}
RELEASE_TAG=v"${VERSION}"
IMAGE_REGISTRY=${IMAGE_REGISTRY:-"quay.io"} # TODO: Add project name after registry

COCKROACH_TAG=v$(yq '.appVersion' "${COCKROACH_CHART}"/Chart.yaml)
QUAY_PROJECT="cockraochdb"
STABLE_CHANNEL=stable-v"$(cut -d'.' -f1 <<<"${VERSION}")".x

function update_olm_operator() {
    valuesJSON=$(yq -p yaml -o json "${COCKROACH_CHART}"/values.yaml | jq tostring)

    sed -i '' 's|VALUES_PLACEHOLDER|'"${valuesJSON}"'|g' "${stableCSV}"

    sed -i '' 's|RELEASE_TAG|'"${RELEASE_TAG}"'|g' "${stableCSV}"

    sed -i '' 's|VERSION|'"${VERSION}"'|g' "${stableCSV}"

    sed -i '' 's|COCKROACH_TAG|'"${COCKROACH_TAG}"'|g' "${stableCSV}"

    sed -i '' 's|IMAGE_REGISTRY|'"${IMAGE_REGISTRY}"'|g' "${stableCSV}"

    sed -i '' 's|STABLE_CHANNEL|'"${STABLE_CHANNEL}"'|g' "${bundleDockerfile}"

    sed -i '' 's|STABLE_CHANNEL|'"${STABLE_CHANNEL}"'|g' "${metaAnnotations}"
}

function release_olm_operator() {
    # TODO: get docker credentials and login

    make build-cockroachdb
    make push-cockroachdb

    make build-ocp-catalog
    make push-ocp-catalog
}

function release_opm_catalogSource() {
    # opm index add --overwrite-latest --container-tool=docker --bundles=quay.io/"${QUAY_PROJECT}"/cockroach-operator-bundle:"${VERSION}" \
    # --tag quay.io/"${QUAY_PROJECT}"/cockroach-operator:"${VERSION}"

    # docker push quay.io/"${QUAY_PROJECT}"/cockroach-operator:"${VERSION}"

    opm index add --overwrite-latest --container-tool=docker --bundles=hemanrnjn/cockroach-operator-bundle:"${VERSION}" \
    --tag hemanrnjn/cockroach-operator:"${VERSION}"

    docker push hemanrnjn/cockroach-operator:"${VERSION}"
}

# function create_release_bundle_pr() {

# }

update_olm_operator
release_olm_operator
release_opm_catalogSource