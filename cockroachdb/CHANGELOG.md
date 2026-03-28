# CockroachDB Helm Chart CHANGELOG

All notable changes to the CockroachDB Helm chart will be documented in this file.

## [Unreleased]
### Added
- Added `tls.enableSighupRotation` configuration option to enable zero-downtime certificate rotation using SIGHUP signal (CRDB-42772).
  - When `tls.enableSighupRotation: true`, node certificates are mounted directly from Kubernetes secrets with 0440 permissions.
  - Supports live certificate reload via SIGHUP without pod restarts.
  - Includes certificate rotation documentation in README.md with deployment examples and technical details.