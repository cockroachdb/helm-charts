#!/usr/bin/env bash

set -euox pipefail

build/tester.sh ct lint --config build/ct.yaml --all
