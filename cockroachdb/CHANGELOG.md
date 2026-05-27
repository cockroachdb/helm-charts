# CockroachDB Helm Chart CHANGELOG

All notable changes to the CockroachDB Helm chart will be documented in this file.

## [21.0.0] 2026-05-25
### Changed
  - Updated the default CockroachDB image version from `v26.1.4` to `v26.2.0`.

## [20.0.5] 2026-05-12
### Changed
  - Updated the default CockroachDB image version from `v26.1.3` to `v26.1.4`.

### Added
  - Added `tls.enableSighupRotation` configuration option to enable zero-downtime certificate rotation using SIGHUP signal.
    - When enabled, node certificates are mounted directly from Kubernetes secrets with 0440 permissions.
    - Supports live certificate reload via SIGHUP without pod restarts.
    - Only compatible with externally managed certificates (user-provided or cert-manager). Not compatible with self-signed certificates.
    - **Backward compatible**: This is an opt-in feature (defaults to `false`). Existing deployments are unaffected and will continue to use the traditional certificate rotation approach with pod restarts.

  - **Additional Subject Alternative Names (SANs) for Self-Signer Certificates (CRDB-55385)**
    - Added `tls.certs.selfSigner.additionalSANs` configuration option to allow custom hostnames and IP addresses in node certificates
    - Enables SSL certificate validation (`sslmode=verify-full`) when routing traffic through load balancers
    - Supports Physical Cluster Replication (PCR) scenarios where replication traffic goes through external endpoints
    - Additional SANs are automatically included in both initial certificate generation and subsequent rotations
    - Configuration example:
      ```yaml
      tls:
        certs:
          selfSigner:
            additionalSANs:
              - "my-loadbalancer.example.com"
              - "192.168.1.100"
      ```
