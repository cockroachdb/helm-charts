# CockroachDB Parent Helm Chart

This Helm chart installs both CockroachDB and its Operator using Helm Spray for proper dependency management and installation order.

## Prerequisites

- Kubernetes 1.16+
- Helm 3.0+
- [Helm Spray Plugin](https://github.com/ThalesGroup/helm-spray)

## Installing the Helm Spray Plugin

```bash
helm plugin install https://github.com/ThalesGroup/helm-spray
```

## Installing the Chart

To install the chart with the release name `my-cockroachdb`:

```bash
# Install using Helm Spray
helm spray -n cockroachdb-ns --create-namespace --timeout 10m ./cockroachdb-parent

# Regular helm install (if not using spray)
helm install -n cockroachdb-ns --create-namespace my-cockroachdb ./cockroachdb-parent
```

## Architecture

This parent chart includes three sub-charts:

1. **operator** - The CockroachDB Operator chart that installs first
2. **crdb-self-signer** - TLS certificate management for CockroachDB
3. **cockroachdb** - The CockroachDB database chart that installs after dependencies are ready

The chart uses both Helm hooks and Helm Spray annotations to ensure proper installation order.

## Configuration

The following table lists the configurable parameters of the chart and their default values.

| Parameter | Description | Default |
| --------- | ----------- | ------- |
| `global.environment` | Global environment setting | `production` |
| `operator.enabled` | Enable/disable operator installation | `true` |
| `crdb-self-signer.enabled` | Enable/disable TLS certificate management | `true` |
| `crdb-self-signer.tls.enabled` | Enable/disable TLS for CockroachDB | `true` |
| `cockroachdb.enabled` | Enable/disable cockroachdb installation | `true` |

You can specify each parameter using the `--set` flag when installing the chart:

```bash
helm spray -n cockroachdb-ns --create-namespace ./cockroachdb-parent --set global.environment=development
```

Alternatively, a YAML file that specifies the values for the parameters can be provided:

```bash
helm spray -n cockroachdb-ns --create-namespace ./cockroachdb-parent -f values-override.yaml
```

## Uninstalling the Chart

To uninstall/delete the `my-cockroachdb` deployment:

```bash
helm uninstall -n cockroachdb-ns my-cockroachdb
```

## Additional Resources

- [CockroachDB Documentation](https://www.cockroachlabs.com/docs/)
- [Helm Spray Documentation](https://github.com/ThalesGroup/helm-spray) 