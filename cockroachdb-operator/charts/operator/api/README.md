# CockroachDB Operator API reference

The [`v1beta1`](v1beta1) package is the public Go type reference for the
`CrdbCluster` and `CrdbNode` resources supported by the self-hosted CockroachDB
Operator.

The CRD YAML in [`../manifests/crds`](../manifests/crds) is authoritative for
Kubernetes validation. This package is maintained in this repository so
customers can review the consumer-facing API without access to the closed-source
operator implementation. It is not a generated Kubernetes client.

## Deprecated `CrdbNodeSpec` fields

> [!WARNING]
> Do not use deprecated fields in new `CrdbCluster` or `CrdbNode`
> configurations. The Kubernetes CRDs retain them only for compatibility with
> existing resources, but they are intentionally omitted from this Go API
> reference. Migrate existing resources to the supported replacements and then
> remove the deprecated fields. Do not configure both forms at the same time.

The paths below are relative to `CrdbNodeSpec` (`spec.template.spec` on a
`CrdbCluster`, or `spec` on a `CrdbNode`):

| Deprecated field | Migrate to |
| --- | --- |
| `podAnnotations` | `podTemplate.metadata.annotations` |
| `podLabels` | `podTemplate.metadata.labels` |
| `env` | The `cockroachdb` container's `podTemplate.spec.containers[].env` |
| `resourceRequirements` | The `cockroachdb` container's `podTemplate.spec.containers[].resources` |
| `serviceAccountName` | `podTemplate.spec.serviceAccountName` |
| `sideCars.initContainers` | `podTemplate.spec.initContainers` |
| `sideCars.containers` | `podTemplate.spec.containers` |
| `sideCars.volumes` | `podTemplate.spec.volumes` |
| `topologySpreadConstraints` | `podTemplate.spec.topologySpreadConstraints` |
| `flags` | `startFlags` |
| `localityLabels` | `localityMappings` |
| `terminationGracePeriod` | `podTemplate.spec.terminationGracePeriodSeconds` |
| `tolerations` | `podTemplate.spec.tolerations` |
| `nodeSelector` | `podTemplate.spec.nodeSelector` |
| `affinity` | `podTemplate.spec.affinity` |
| `readinessProbe` | The `cockroachdb` container's `podTemplate.spec.containers[].readinessProbe` |
