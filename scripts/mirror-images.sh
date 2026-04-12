#!/usr/bin/env bash
#
# Mirror all container images required by CockroachDB Helm charts to an
# internal registry. Designed for air-gapped and restricted environments.
#
# Prerequisites: crane (https://github.com/google/go-containerregistry/tree/main/cmd/crane)
#   or skopeo (https://github.com/containers/skopeo)
#
# Usage:
#   ./scripts/mirror-images.sh --target-registry my-registry.internal.io
#   ./scripts/mirror-images.sh --target-registry my-registry.internal.io --source-file cockroachdb-parent/images.txt
#   ./scripts/mirror-images.sh --target-registry my-registry.internal.io --tool skopeo
#   ./scripts/mirror-images.sh --target-registry my-registry.internal.io --dry-run

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TARGET_REGISTRY=""
SOURCE_FILE="${REPO_ROOT}/cockroachdb-parent/images.txt"
TOOL="crane"
DRY_RUN=false

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Mirror CockroachDB Helm chart images to an internal registry.

Options:
  --target-registry REGISTRY   Target registry to push images to (required)
  --source-file FILE           Path to images.txt manifest (default: cockroachdb-parent/images.txt)
  --tool TOOL                  Tool to use for mirroring: crane or skopeo (default: crane)
  --dry-run                    Print commands without executing
  -h, --help                   Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target-registry) TARGET_REGISTRY="$2"; shift 2 ;;
    --source-file)     SOURCE_FILE="$2"; shift 2 ;;
    --tool)            TOOL="$2"; shift 2 ;;
    --dry-run)         DRY_RUN=true; shift ;;
    -h|--help)         usage; exit 0 ;;
    *)                 echo "Unknown option: $1"; usage; exit 1 ;;
  esac
done

if [[ -z "${TARGET_REGISTRY}" ]]; then
  echo "Error: --target-registry is required"
  usage
  exit 1
fi

if [[ ! -f "${SOURCE_FILE}" ]]; then
  echo "Error: source file not found: ${SOURCE_FILE}"
  exit 1
fi

if [[ "${TOOL}" != "crane" && "${TOOL}" != "skopeo" ]]; then
  echo "Error: --tool must be 'crane' or 'skopeo'"
  exit 1
fi

if ! command -v "${TOOL}" &>/dev/null; then
  echo "Error: ${TOOL} not found in PATH"
  echo "Install instructions:"
  if [[ "${TOOL}" == "crane" ]]; then
    echo "  https://github.com/google/go-containerregistry/tree/main/cmd/crane#installation"
  else
    echo "  https://github.com/containers/skopeo/blob/main/install.md"
  fi
  exit 1
fi

# Compute the target image reference from a source reference.
# Strips the source registry and prepends the target registry.
# For digest references (@sha256:...), preserves the digest.
compute_target() {
  local src="$1"
  local repo_and_ref

  # Strip the registry prefix (everything before the first slash that isn't part of a path)
  # Examples:
  #   us-docker.pkg.dev/releases-prod/self-hosted/cockroachdb-operator@sha256:abc -> cockroachdb-operator@sha256:abc
  #   docker.io/cockroachdb/cockroach:v26.1.2 -> cockroachdb/cockroach:v26.1.2
  #   gcr.io/cockroachlabs-helm-charts/cockroach-self-signer-cert:1.9 -> cockroach-self-signer-cert:1.9
  case "${src}" in
    us-docker.pkg.dev/*/*/*)
      # Google Artifact Registry: us-docker.pkg.dev/project/repo/image
      # Strip us-docker.pkg.dev/project/repo/ to keep only image:tag or image@digest
      local without_host="${src#us-docker.pkg.dev/}"
      local without_project="${without_host#*/}"
      repo_and_ref="${without_project#*/}"
      ;;
    gcr.io/*)
      # GCR: gcr.io/project/image
      repo_and_ref="${src#gcr.io/*/}"
      ;;
    docker.io/*)
      repo_and_ref="${src#docker.io/}"
      ;;
    *)
      # Fallback: strip first path segment as registry
      repo_and_ref="${src#*/}"
      ;;
  esac

  echo "${TARGET_REGISTRY}/${repo_and_ref}"
}

echo "Mirroring images from: ${SOURCE_FILE}"
echo "Target registry: ${TARGET_REGISTRY}"
echo "Tool: ${TOOL}"
echo ""

SUCCESS=0
FAILED=0

while IFS= read -r line; do
  # Skip comments and blank lines
  [[ "${line}" =~ ^[[:space:]]*# ]] && continue
  [[ -z "${line// /}" ]] && continue

  src="${line}"
  dst="$(compute_target "${src}")"

  echo "Mirroring: ${src}"
  echo "      --> ${dst}"

  if [[ "${DRY_RUN}" == true ]]; then
    if [[ "${TOOL}" == "crane" ]]; then
      echo "  [dry-run] crane copy ${src} ${dst}"
    else
      echo "  [dry-run] skopeo copy docker://${src} docker://${dst}"
    fi
    echo ""
    continue
  fi

  if [[ "${TOOL}" == "crane" ]]; then
    if crane copy "${src}" "${dst}"; then
      echo "  OK"
      ((SUCCESS++))
    else
      echo "  FAILED"
      ((FAILED++))
    fi
  else
    if skopeo copy "docker://${src}" "docker://${dst}"; then
      echo "  OK"
      ((SUCCESS++))
    else
      echo "  FAILED"
      ((FAILED++))
    fi
  fi
  echo ""
done < "${SOURCE_FILE}"

echo "================================"
echo "Mirroring complete: ${SUCCESS} succeeded, ${FAILED} failed"

if [[ "${FAILED}" -gt 0 ]]; then
  exit 1
fi
