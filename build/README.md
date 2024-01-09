# Building, Testing and Releasing

The scripts is used for testing, building and deploying.

### Building

The build image is a basic image containing Helm. To use it:

```
/.../helm-charts $ helm package cockroachdb
```

## Building Catalog Images for helm chart operator

- Export the following environment while running locally:
    QUAY_DOCKER_USERNAME, QUAY_DOCKER_TOKEN
- Run `make prepare_bundle` to prepare the bundle to be pushed on community-operators. The `/bundle` directory at the root folder contains the template for the bundle which will get updated after executing the above command. The same directory can then be used to manually place under the new version in the community-operators repo. NOTE: This will not build the helm chart operator or the catalog bundle image
- Run `make build-and-release-olm-operator` to prepare the bundle as well as build and push the helm chart operator image and catalog bundle image to the registry provided above as environment variables.
