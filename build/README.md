# Building, Testing and Releasing

The scripts and docker images used for testing, building and deploying.

Installing docker is a prerequisite. The instructions differ depending on the
environment. Docker is comprised of two parts: the daemon server which runs on
Linux and accepts commands, and the client which is a Go program capable of
running on MacOS, all Unix variants and Windows.


## Docker Installation

Follow the [Docker install
instructions](https://docs.docker.com/engine/installation/).


## Available Images

### Building

The build image is a basic image containing Helm. To use it:

```
/.../helm-charts $ ./build/builder.sh helm package cockroachdb
```


## Upgrading / Extending the Builder Image

- Edit `build/builder/Dockerfile` as desired
- Run `build/builder.sh init` to test -- this will build the image locally. The result of `init` is a docker image version which you can subsequently stick into the `version` variable inside the `builder.sh` script for testing locally.
- Once you are happy with the result, run `build/builder.sh push` which pushes your image towards Docker hub, so that it becomes available to others. The result is again a version number, which you then *must* copy back into `build/builder.sh`. Then commit the change to both Dockerfile and `build/builder.sh` and submit a PR.


## Building Catalog Images for helm chart operator

- Export the following environment while running locally:
    QUAY_DOCKER_USERNAME, QUAY_DOCKER_TOKEN
- Run `make prepare_bundle` to prepare the bundle to be pushed on community-operators. The `/bundle` directory at the root folder contains the template for the bundle which will get updated after executing the above command. The same directory can then be used to manually place under the new version in the community-operators repo. NOTE: This will not build the helm chart operator or the catalog bundle image
- Run `make build-and-release-olm-operator` to prepare the bundle as well as build and push the helm chart operator image and catalog bundle image to the registry provided above as environment variables.