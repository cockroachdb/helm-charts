# Direct `CrdbCluster` examples

These files are Kubernetes manifests for non-Helm installations. They can be
applied with `kubectl`; they are not Helm values files.

All examples use the `cockroachdb` namespace and the `cockroachdb`
ServiceAccount. Apply the shared RBAC before applying one of the cluster
examples:

```shell
kubectl apply \
  -f cockroachdb-parent/charts/operator/manifests/examples/crdb/rbac.yaml
```

Choose and customize one example:

- [`insecure.yaml`](insecure.yaml): local testing without TLS.
- [`secure.yaml`](secure.yaml): TLS using certificates provisioned ahead of
  time.
- [`multi-region.yaml`](multi-region.yaml): one logical CockroachDB cluster
  spanning multiple Kubernetes clusters.
- [`cmek.yaml`](cmek.yaml): GCP Cloud KMS-backed encryption at rest.
- [`wal-failover.yaml`](wal-failover.yaml): a dedicated WAL failover volume.
- [`pod-template.yaml`](pod-template.yaml): supported Pod template overrides.

Before applying an example:

1. Install the operator and the `CrdbCluster` and `CrdbNode` CRDs.
2. Set the operator's `CLOUD_REGION` to the `regions[].code` reconciled by that
   Kubernetes cluster.
3. Replace example namespaces, regions, domains, StorageClasses, images, and
   resource sizes for the target environment.
4. Label Kubernetes Nodes with the matching
   `topology.kubernetes.io/region`.
5. Provision every referenced Secret and ConfigMap first. The examples contain
   comments describing the required keys.

For example:

```shell
kubectl apply \
  -f cockroachdb-parent/charts/operator/manifests/examples/crdb/insecure.yaml

kubectl -n cockroachdb get crdbclusters,crdbnodes,pods
```

The insecure example is for testing only. Use TLS and production-appropriate
storage, resources, topology, and certificate management for production.
