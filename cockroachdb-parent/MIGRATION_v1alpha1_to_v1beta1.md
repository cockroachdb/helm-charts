# CockroachDB Operator API Version Migration (v1alpha1 → v1beta1)

## Overview

CockroachDB Operator is transitioning from `v1alpha1` to `v1beta1` API version. This document provides essential upgrade instructions.

**Current Version**: `25.4.3-preview+1` (Multi-version support)

---

## Before You Upgrade

### 🚨 Critical Warning

**NEVER DELETE THE CRD**
- **DO NOT delete CRDs** if you encounter upgrade issues
- Deleting CRDs will **permanently delete all your CockroachDB clusters and data**
- If you have upgrade issues, check logs and contact support – do not delete CRDs

---

## User Scenarios

### New Users (Fresh Installation)
If you're installing for the first time:
- ✅ Install normally - no special steps needed
- ✅ Pre-upgrade validation will automatically detect new installation and skip unnecessary checks

### Existing Users (Upgrading from v1alpha1)
If you have existing CockroachDB clusters:
- ✅ Follow the two-step upgrade process below
- ✅ Pre-upgrade validation will automatically:
  - Verify CRD supports v1beta1
  - Rewrite resources to v1beta1 storage format
  - Validate API access
- ✅ Zero downtime - your cluster continues running during the upgrade

---

## Upgrade Instructions

### ⚠️ Required Upgrade Order

**The operator MUST be upgraded before the CockroachDB chart.**

Why? The CockroachDB chart now uses `v1beta1` templates, which requires the operator to enable `v1beta1` support in the CRD first.

### Step-by-Step Upgrade

```bash
# Step 1: Upgrade operator first
helm upgrade <operator-release> ./cockroachdb-parent/charts/operator -n <namespace>

# Step 2: Then upgrade CockroachDB chart
helm upgrade <cockroachdb-release> ./cockroachdb-parent/charts/cockroachdb -n <namespace>
```

**Important**: Both steps are required. Even if you don't have CockroachDB chart value changes, you must upgrade it to update Helm's stored manifest to v1beta1.

### Post-Upgrade

Clear your kubectl cache:
```bash
rm -rf ~/.kube/cache
```

---

## What Happens During Upgrade

1. **Operator Upgrade**: Adds v1beta1 support to CRD (both v1alpha1 and v1beta1 are served, v1beta1 is storage version)
2. **CockroachDB Chart Upgrade**: Updates templates to v1beta1, pre-upgrade hook validates and rewrites resources
3. **Automatic Validation**: Pre-upgrade hook ensures everything is ready before proceeding

---

## Common Issues

### "UPGRADE BLOCKED - CRD does not support v1beta1"

**Cause**: Trying to upgrade CockroachDB chart before operator

**Solution**: Upgrade operator first (Step 1 above), then retry CockroachDB chart upgrade

---

### "Cannot access CrdbCluster via v1beta1 API"

**Cause**: Operator not running or CRD configuration issue

**Solution**:
1. Check operator status: `kubectl get pods -n <namespace> -l app.kubernetes.io/name=cockroach-operator`
2. Check operator logs: `kubectl logs -n <namespace> -l app.kubernetes.io/name=cockroach-operator`
3. Verify CRD: `kubectl get crd crdbclusters.crdb.cockroachlabs.com -o yaml`

---

## Verification

After upgrade, verify everything is working:

```bash
# Check CRD configuration
kubectl get crd crdbclusters.crdb.cockroachlabs.com \
  -o jsonpath='{.spec.versions[?(@.storage==true)].name}'
# Expected output: v1beta1

# Check your clusters
kubectl get crdbcluster -n <namespace>

# Verify Helm manifest uses v1beta1
helm get manifest <release> -n <namespace> | grep "apiVersion: crdb.cockroachlabs.com"
# Expected: v1beta1
```

---

## FAQ

**Q: Will this cause downtime?**  
A: No. The migration is zero-downtime. Resources remain accessible during upgrade.

**Q: Do I need to do anything manually?**  
A: No. Just follow the two-step upgrade order. Pre-upgrade hooks handle everything else automatically.

**Q: What if I only upgrade the operator?**  
A: You must also upgrade the CockroachDB chart (Step 2). This updates Helm's manifest to v1beta1, which is required for future upgrades.

---

## Support

If you encounter issues:

**🚨 IMPORTANT: Never delete CRDs to "fix" upgrade issues - this will destroy all your data!**

**Check pre-upgrade job logs:**
```bash
kubectl logs job/<release>-pre-upgrade-validation -n <namespace>
```

**Check operator logs:**
```bash
kubectl logs -n <namespace> -l app.kubernetes.io/name=cockroach-operator --tail=50
```

**Check for events:**
```bash
kubectl get events -n <namespace> --sort-by='.lastTimestamp'
```

---

## Quick Reference

**Before upgrade:**
- ⚠️ Never delete CRDs (will delete all data)
- ⚠️ Upgrade operator first, then CockroachDB chart

**Upgrade commands:**
```bash
# Step 1
helm upgrade <operator-release> ./cockroachdb-parent/charts/operator -n <namespace>

# Step 2
helm upgrade <cockroachdb-release> ./cockroachdb-parent/charts/cockroachdb -n <namespace>
```

**After upgrade:**
```bash
# Clear kubectl cache
rm -rf ~/.kube/cache

# Verify
kubectl get crdbcluster -n <namespace>
```

**If issues occur:**
- Check job logs: `kubectl logs job/<release>-pre-upgrade-validation`
- Check operator logs
- DO NOT delete CRDs
