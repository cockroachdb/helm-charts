# Contributing

Contributions are welcome!

For every change, please increment the `version` contained in
[Chart.yaml](https://github.com/cockroachdb/helm-charts/blob/master/cockroachdb/Chart.yaml).
The `version` roughly follows the [SEMVER](https://semver.org/) versioning 
pattern. For changes which do not affect backwards compatibility, the PATCH or
MINOR version must be incremented, e.g. `4.1.3` -> `4.1.4`. For changes which
affect the backwards compatibility of the chart, the major version must be
incremented, e.g. `4.1.3` -> `5.0.0`. Examples of changes which affect backwards
compatibility include any major version releases of CockroachDB, as well as any
breaking changes to the CockroachDB chart templates.

