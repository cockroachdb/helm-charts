REPOSITORY ?= cockroachdb/cockroach-self-signer-cert
TAG ?= $(shell git rev-parse HEAD)

.DEFAULT_GOAL := all
all: build

.PHONY: build
build:
	build/make.sh

.PHONY: lint
lint:
	build/lint.sh

.PHONY: release
release:
	build/release.sh

.PHONY: clean
clean:
	rm -r build/artifacts/

build-self-signer:
	docker build -f build/docker-image/Dockerfile -t ${REPOSITORY}:${TAG} .

push-self-signer:
	docker push ${REPOSITORY}:${TAG}
