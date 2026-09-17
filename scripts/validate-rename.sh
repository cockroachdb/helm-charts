#!/usr/bin/env bash
# Validates the cockroachdb -> cockroachdb-legacy and cockroachdb-parent -> cockroachdb-operator
# folder rename. Every step below is a specific class of breakage the rename could introduce.
#
# Usage: ./scripts/validate-rename.sh [--skip-k3d]
#
# Exits non-zero on the first failure. Intended to run in CI and locally before
# opening a PR that touches these folders. Assumes bin/helm, bin/yq, bin/kubectl,
# and (unless --skip-k3d) bin/k3d are already installed via `make bin`.

set -euo pipefail

SKIP_K3D=false
for arg in "$@"; do
  case "$arg" in
    --skip-k3d) SKIP_K3D=true ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

section() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
fail()    { printf '\033[1;31mFAIL:\033[0m %s\n' "$*" >&2; exit 1; }
ok()      { printf '\033[1;32mOK:\033[0m %s\n' "$*"; }

# 1. Static grep guard.
#    No non-historical `cockroachdb-parent` string should remain. The only allowed
#    survivors are `cockroachdb-parent-<digit>` in CHANGELOG.md (release headings)
#    and MIGRATION_v1alpha1_to_v1beta1.md (git checkout of historical preview tags).
section "1/8  Static guard: no stray cockroachdb-parent references"
STRAY_PARENT=$(git grep -nP 'cockroachdb-parent(?!-[0-9])' -- \
  ':!CHANGELOG.md' \
  ':!cockroachdb-operator/MIGRATION_v1alpha1_to_v1beta1.md' \
  ':!.github/workflows/ci.yaml' \
  ':!scripts/validate-rename.sh' \
  || true)
# Excluded above:
#   CHANGELOG.md — historical `[cockroachdb-parent-<version>]` release headings
#   MIGRATION_v1alpha1_to_v1beta1.md — historical `./cockroachdb-parent/charts/…`
#       helm paths used against pre-rename release tags
#   ci.yaml — one-release-cycle fallback that reads base chart versions from
#       either cockroachdb-operator/… or cockroachdb-parent/…; delete the fallback
#       in a follow-up PR and drop this exclusion.
#   validate-rename.sh — this script names the string in its own comments.
if [[ -n "$STRAY_PARENT" ]]; then
  echo "$STRAY_PARENT"
  fail "found unexpected cockroachdb-parent references above"
fi
ok "no stray cockroachdb-parent references"

# 2. Static grep guard for the top-level `cockroachdb/` folder path.
#    Excludes the *subchart* at cockroachdb-operator/charts/cockroachdb/ (both the
#    files inside it and relative-path references like `../cockroachdb/…` from its
#    sibling operator subchart). Product name, artifact name, and container image
#    references (cockroachdb/cockroach*) are also fine and not matched.
#    Covers TWO shapes:
#      (a) path-with-trailing-file: ./cockroachdb/Chart.yaml, ../../cockroachdb/values.yaml, etc.
#      (b) terminal-token in Go string literals: filepath.Join(root, "cockroachdb"),
#          fmt.Sprintf("%s/cockroachdb", root). This shape was missed by earlier
#          runs of this guard and let real regressions slip through.
section "2/8  Static guard: no stray top-level cockroachdb/ folder path"
STRAY_LEGACY_PATH=$(git grep -nE '(\./|"\.\./\.\./|\[|"|filepath\.(Abs|Join)\([^)]*")cockroachdb/(Chart\.yaml|values\.yaml|values\.schema\.json|templates|README\.md|CHANGELOG\.md|CONTRIBUTING\.md)' \
  -- ':!scripts/validate-rename.sh' \
  ':!cockroachdb-operator/charts/**' \
  ':!build/templates/cockroachdb-operator/charts/**' \
  || true)
STRAY_LEGACY_TOKEN=$(git grep -nE '(filepath\.(Abs|Join)\([^)]*"|"%s/|Sprintf\([^)]*")cockroachdb"' \
  -- ':!scripts/validate-rename.sh' \
  ':!cockroachdb-operator/charts/**' \
  ':!build/templates/cockroachdb-operator/charts/**' \
  || true)
# Shape (c): bare `./cockroachdb` used as a folder path in shell commands,
# markdown links, or shell-style exports. Matches when the path is followed by
# end-of-line, whitespace, quote, backtick, or a closing paren — but NOT when
# followed by `-` (which would be cockroachdb-legacy/cockroachdb-operator/
# cockroachdb-chart) or `/` (already covered by shape (a)) or `.` (covers
# refs like `.io/cockroachdb.`). This is the guard that would have caught
# `helm install crdb ./cockroachdb` and `export ORIGINAL_CHART="./cockroachdb"`.
STRAY_LEGACY_BARE=$(git grep -nP '(?<![-\w])\./cockroachdb(?![-\w/.])' \
  -- ':!scripts/validate-rename.sh' \
  ':!CHANGELOG.md' \
  ':!cockroachdb-operator/charts/**' \
  ':!build/templates/cockroachdb-operator/charts/**' \
  || true)
# CHANGELOG.md excluded because the customer-facing rename entry deliberately
# shows the pre-rename command as a "Before" example.
STRAY_LEGACY="$STRAY_LEGACY_PATH"$'\n'"$STRAY_LEGACY_TOKEN"$'\n'"$STRAY_LEGACY_BARE"
STRAY_LEGACY=$(printf '%s' "$STRAY_LEGACY" | grep -v '^$' || true)
if [[ -n "$STRAY_LEGACY" ]]; then
  echo "$STRAY_LEGACY"
  fail "found unexpected top-level cockroachdb/ folder path references above"
fi
ok "no stray legacy folder path references"

# 3. Old folders must not exist.
section "3/8  Old folder paths are gone"
for old in cockroachdb cockroachdb-parent build/templates/cockroachdb build/templates/cockroachdb-parent; do
  if [[ -e "$old" ]]; then
    fail "old path still exists: $old"
  fi
done
ok "old folders removed"

# 4. New folders exist with expected contents.
section "4/8  New folder paths exist with core files"
for f in \
  cockroachdb-legacy/Chart.yaml \
  cockroachdb-legacy/values.yaml \
  cockroachdb-legacy/templates \
  cockroachdb-operator/Chart.yaml \
  cockroachdb-operator/values.yaml \
  cockroachdb-operator/charts/cockroachdb/Chart.yaml \
  cockroachdb-operator/charts/operator/Chart.yaml \
  build/templates/cockroachdb-legacy/README.md \
  build/templates/cockroachdb-operator/Chart.yaml; do
  [[ -e "$f" ]] || fail "missing expected path: $f"
done
ok "new folders present"

# 5. Generated-source parity. The rename itself introduces a large diff vs. HEAD,
#    so we check idempotency by comparing full-file hashes across two runs
#    instead of `git status --porcelain` (which only reports changed-vs-index and
#    would silently pass a bug where the same wrong content is written twice).
section "5/8  go run build/build.go generate is idempotent"
go run build/build.go generate
FIRST_HASH=$(find cockroachdb-legacy cockroachdb-operator -type f -not -name '*.tgz' -print0 | sort -z | xargs -0 shasum -a 256 | shasum -a 256)
go run build/build.go generate
SECOND_HASH=$(find cockroachdb-legacy cockroachdb-operator -type f -not -name '*.tgz' -print0 | sort -z | xargs -0 shasum -a 256 | shasum -a 256)
if [[ "$FIRST_HASH" != "$SECOND_HASH" ]]; then
  echo "first pass:  $FIRST_HASH"
  echo "second pass: $SECOND_HASH"
  fail "generate is not idempotent; second pass produced different content"
fi
ok "generate is idempotent (hash-stable across two runs)"

# 6. Helm hygiene: lint the two renamed charts and their subcharts, and confirm
#    the umbrella chart's dependencies resolve from the renamed folder.
section "6/8  helm lint + dependency update"
bin/helm lint cockroachdb-legacy
bin/helm lint cockroachdb-operator/charts/cockroachdb
bin/helm lint cockroachdb-operator/charts/operator
bin/helm dependency update ./cockroachdb-operator
# Chart.lock churns on the `generated` timestamp — undo that specific noise.
git checkout -- cockroachdb-operator/Chart.lock 2>/dev/null || true
ok "helm lint + dependency resolution succeeded"

# 7. Go tests exercising the renamed paths.
section "7/8  Go tests: ./build/... and ./tests/template/..."
go test ./build/...
go test ./tests/template/...
ok "go tests passed"

# 8. k3d smoke deploy (skippable). This is the true end-to-end proof that
#    installing from the renamed folder still produces a running cluster.
if [[ "$SKIP_K3D" == "true" ]]; then
  echo "==> 8/8  k3d smoke deploy: SKIPPED (--skip-k3d)"
  echo ""
  echo "All non-k3d validation passed. Run without --skip-k3d before opening the PR."
  exit 0
fi

section "8/8  k3d smoke: install both charts from renamed folders and reach Ready"
CLUSTER=rename-validation
trap 'bin/k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true' EXIT

# 8a. Legacy StatefulSet-mode chart smoke.
bin/k3d cluster create "$CLUSTER" --wait --timeout 5m
bin/kubectl create namespace crdb-legacy-smoke
bin/helm install crdb-legacy ./cockroachdb-legacy -n crdb-legacy-smoke --wait --timeout 10m
bin/kubectl -n crdb-legacy-smoke rollout status statefulset/crdb-legacy-cockroachdb --timeout 5m
bin/helm uninstall crdb-legacy -n crdb-legacy-smoke
bin/kubectl delete namespace crdb-legacy-smoke

# 8b. Operator umbrella chart smoke.
bin/kubectl create namespace crdb-operator-smoke
bin/helm dependency update ./cockroachdb-operator
bin/helm install crdb-op ./cockroachdb-operator -n crdb-operator-smoke --wait --timeout 15m
# Wait for the CRDB pods the operator provisions.
bin/kubectl -n crdb-operator-smoke wait --for=condition=Ready pod -l app.kubernetes.io/name=cockroachdb --timeout 10m
bin/helm uninstall crdb-op -n crdb-operator-smoke
bin/kubectl delete namespace crdb-operator-smoke

bin/k3d cluster delete "$CLUSTER"
trap - EXIT

ok "k3d smoke deploy succeeded for both renamed charts"
echo ""
echo "All 8 validation steps passed. Safe to open the rename PR."
