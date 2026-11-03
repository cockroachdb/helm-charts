#!/usr/bin/env bash
# Shared by provider cleanup and the scheduled/manual workflow.
export CLEANUP_DRY_RUN="${CLEANUP_DRY_RUN:-false}"

cleanup_parse_args() {
  CLEANUP_ARGS=()
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --dry-run) CLEANUP_DRY_RUN=true ;;
      *) CLEANUP_ARGS+=("$1") ;;
    esac
    shift
  done
}

cleanup_header() {
  printf '\nCleanup scope: %s\n' "$1"
  if [[ "${CLEANUP_DRY_RUN}" == "true" ]]; then
    echo "Mode: dry run (resources will only be listed)"
  else
    echo "Mode: delete after listing selected resources"
  fi
  printf 'TYPE\tNAME\tLOCATION\tID\n'
}

cleanup_report() {
  local kind="$1" resources="$2"
  jq -r --arg kind "${kind}" '.[] | [(.type // $kind), (.name // .infraID),
    (.location // .region // .zone // "global" | split("/")[-1]), (.id // .infraID // "-")] | @tsv' <<< "${resources}"
}

cleanup_change() {
  local mode="RUN"
  [[ "${CLEANUP_DRY_RUN}" == "true" ]] && mode="DRY RUN"
  printf '%s' "${mode}"
  printf ' %q' "$@"
  printf '\n'
  if [[ "${CLEANUP_DRY_RUN}" != "true" ]]; then
    "$@"
  fi
}

cleanup_missing() {
  [[ "$1" == *"matched no objects"* || "$1" == *"No URLs matched"* || "$1" == *"was not found"* || "$1" == *"NOT_FOUND"* ]]
}
