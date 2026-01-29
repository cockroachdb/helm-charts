# CockroachDB Operator API Version Migration (v1alpha1 → v1beta1)

## Overview

The CockroachDB Operator is migrating from `v1alpha1` to `v1beta1` API version.

**Migration phases:**
- **Phase 1 (25.4.3-preview+1)**: Both v1alpha1 and v1beta1 are served, v1beta1 is the storage version
- **Phase 2 (25.4.3-preview+2)**: Only v1beta1 is served, v1alpha1 is disabled

---

## Critical Warning

**Never delete CRDs to fix upgrade issues.** Deleting CRDs will permanently delete all CockroachDB clusters and data. If you have upgrade problems, check logs and contact support.

---

## Who This Applies To

**New users:** Install normally, no special steps needed.

**Existing users:** Follow the two-phase upgrade below. The migration is automatic and zero-downtime - your clusters keep running while resources are migrated to v1beta1.

---

## Upgrade Instructions

### Phase 1: Multi-Version Support (25.4.3-preview+1)

**Important:** Upgrade the operator first, then the CockroachDB chart. The chart uses v1beta1 templates, so the operator needs to enable v1beta1 support in the CRD first.

#### Upgrade Steps

```bash
# Clear kubectl cache first (important!)
rm -rf ~/.kube/cache

# Checkout Phase 1 tag
git checkout cockroachdb-parent-25.4.3-preview+1

# Step 1: Upgrade operator first
helm upgrade <operator-release> ./cockroachdb-parent/charts/operator -n <namespace>

# Step 2: Then upgrade CockroachDB chart
helm upgrade <cockroachdb-release> ./cockroachdb-parent/charts/cockroachdb -n <namespace>
```

**Note:** Both upgrades are required. Even if you have no chart value changes, you must upgrade the CockroachDB chart to update Helm's stored manifest to v1beta1.

**Rollback warning:** After this upgrade, don't rollback to the previous operator version. Your resources are stored as v1beta1 in etcd, and the previous operator doesn't support v1beta1.

#### Verification

After completing Phase 1 upgrade, verify the migration:

```bash
# Check CRD configuration
kubectl get crd crdbclusters.crdb.cockroachlabs.com \
  -o jsonpath='{.spec.versions[?(@.storage==true)].name}'
# Expected output: v1beta1

# Check both versions are served
kubectl get crd crdbclusters.crdb.cockroachlabs.com \
  -o jsonpath='{.spec.versions[?(@.served==true)].name}'
# Expected output: v1alpha1 v1beta1

# Check your clusters
kubectl get crdbcluster -n <namespace>

# Verify Helm manifest uses v1beta1
helm get manifest <release> -n <namespace> | grep "apiVersion: crdb.cockroachlabs.com"
# Expected: v1beta1
```

#### Common Issues

**Issue: "UPGRADE BLOCKED - CRD does not support v1beta1"**

**Cause**: Trying to upgrade CockroachDB chart before operator

**Solution**: Upgrade operator first (Step 1 above), then retry CockroachDB chart upgrade

---

**Issue: "Cannot access CrdbCluster via v1beta1 API"**

**Cause**: Operator not running or CRD configuration issue

**Solution**:
1. Check operator status: `kubectl get pods -n <namespace> -l app.kubernetes.io/name=cockroach-operator`
2. Check operator logs: `kubectl logs -n <namespace> -l app.kubernetes.io/name=cockroach-operator`
3. Verify CRD: `kubectl get crd crdbclusters.crdb.cockroachlabs.com -o yaml`

---

### Phase 2: Disable v1alpha1 (25.4.3-preview+2)

#### Prerequisites

You must complete Phase 1 (both operator and CockroachDB chart) before upgrading to Phase 2. Pre-upgrade validation will block the upgrade if:
- You skipped the CockroachDB chart upgrade in Phase 1
- You're trying to skip Phase 1 entirely
- Your Helm manifests still use v1alpha1

#### Step-by-Step Upgrade

```bash
# Clear kubectl cache first (important!)
rm -rf ~/.kube/cache

# Checkout Phase 2 tag
git checkout cockroachdb-parent-25.4.3-preview+2

# Upgrade operator to Phase 2
helm upgrade <operator-release> ./cockroachdb-parent/charts/operator -n <namespace>
```

The upgrade will:
1. Validate prerequisites (blocks if Helm manifests still use v1alpha1)
2. Disable v1alpha1 serving (only v1beta1 will be served)
3. Automatically migrate all CrdbCluster, CrdbNode, and CrdbTenant resources to v1beta1 storage
4. Update CRD to remove v1alpha1 from stored versions

Check if migration completed (optional):
```bash
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o jsonpath='{.status.storedVersions}'
# Expected: ["v1beta1"]
```

#### Upgrading CockroachDB Charts After Phase 2

The operator must be upgraded to Phase 2 (with storage migration complete) before upgrading CockroachDB charts. Validation will block the chart upgrade if the operator is still on Phase 1 or if storage migration hasn't completed.

#### What If Storage Migration Fails?

If the automatic storage migration fails during operator upgrade:

**Recovery:**
Simply **retry the operator upgrade** - the migration is safe to run multiple times:
```bash
helm upgrade <operator-release> ./cockroachdb-parent/charts/operator -n <namespace>
```

The migration will run again and complete successfully. After operator upgrade completes, you can proceed with CockroachDB chart upgrades normally.

#### Verification

After completing Phase 2 upgrade, verify the migration:

```bash
# 1. Check only v1beta1 is served
kubectl get crd crdbclusters.crdb.cockroachlabs.com \
  -o jsonpath='{.spec.versions[?(@.served==true)].name}'
# Expected output: v1beta1 (only)

# 2. Verify storage migration completed
kubectl get crd crdbclusters.crdb.cockroachlabs.com \
  -o jsonpath='{.status.storedVersions}'
# Expected output: ["v1beta1"]

# 3. Verify v1alpha1 API is not accessible
kubectl get crdbclusters.v1alpha1.crdb.cockroachlabs.com -n <namespace>
# Expected: error (version not served)

# 4. Verify v1beta1 API works
kubectl get crdbclusters.v1beta1.crdb.cockroachlabs.com -n <namespace>
# Expected: success

# 5. Check your clusters
kubectl get crdbcluster -n <namespace>
```

#### Common Issues

Most errors tell you exactly what to do. Here are the common ones:

**"UPGRADE BLOCKED - CRD does not exist"** or **"CRD does not support v1beta1"**  
Upgrade the operator first:
```bash
helm upgrade <operator-release> ./cockroachdb-parent/charts/operator -n <namespace>
```

**"Storage version migration has not been completed"**  
Retry the operator upgrade:
```bash
helm upgrade <operator-release> ./cockroachdb-parent/charts/operator -n <namespace>
```

Then retry the CockroachDB chart upgrade.

---

## FAQ

**Will this cause downtime?**  
No, both phases are zero-downtime upgrades.

**Can I skip Phase 1?**  
No, you must go through Phase 1 first. Phase 2 will block if you skip it.

**What if I only upgrade the operator in Phase 1?**  
You must also upgrade the CockroachDB chart to update Helm's manifest to v1beta1, which Phase 2 requires.

**Can I use v1alpha1 YAML files in Phase 1?**  
Yes, if using kubectl/Go clients (not Helm). The API server converts automatically, but you must switch to v1beta1 before Phase 2.

**Can I rollback after Phase 2?**  
Yes. Phase 1 supports v1beta1, so you can rollback with `helm rollback <operator-release>` and it will continue serving your v1beta1 resources.

---

## Support

If you encounter issues:

**Never delete CRDs** - this will permanently destroy all your data.

If upgrade issues occur:
1. Read the error message - it provides specific recovery steps
2. Most common fix: Retry the operator upgrade
3. Check operator logs if needed:
   ```bash
   kubectl logs -n <namespace> -l app.kubernetes.io/name=cockroach-operator --tail=50
   ```

---

## Quick Reference

### Phase 1
```bash
git checkout cockroachdb-parent-25.4.3-preview+1
rm -rf ~/.kube/cache

helm upgrade <operator-release> ./cockroachdb-parent/charts/operator -n <namespace>
helm upgrade <cockroachdb-release> ./cockroachdb-parent/charts/cockroachdb -n <namespace>
```

### Phase 2
```bash
git checkout cockroachdb-parent-25.4.3-preview+2
rm -rf ~/.kube/cache

helm upgrade <operator-release> ./cockroachdb-parent/charts/operator -n <namespace>
```

Verify migration:
```bash
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o jsonpath='{.status.storedVersions}'
# Expected: ["v1beta1"]
```
