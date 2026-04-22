# CockroachDB Helm Chart CHANGELOG

All notable changes to the CockroachDB Helm chart will be documented in this file.

## [Unreleased]
### Added
  - Added `tls.enableSighupRotation` configuration option to enable zero-downtime certificate rotation using SIGHUP signal.
    - When enabled, node certificates are mounted directly from Kubernetes secrets with 0440 permissions.
    - Supports live certificate reload via SIGHUP without pod restarts.
    - Only compatible with externally managed certificates (user-provided or cert-manager). Not compatible with self-signed certificates.
    - **Backward compatible**: This is an opt-in feature (defaults to `false`). Existing deployments are unaffected and will continue to use the traditional certificate rotation approach with pod restarts.