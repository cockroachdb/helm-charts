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

