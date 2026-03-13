# Changelog

All notable changes to the CockroachDB Helm chart will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Added

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
