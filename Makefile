REPOSITORY ?= gcr.io/cockroachdb/cockroach-self-signer-cert
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

install-cockroach:
	sudo apt-get install wget -y
	wget https://binaries.cockroachdb.com/cockroach-v20.2.5.linux-amd64.tgz
	tar zxf cockroach-v20.2.5.linux-amd64.tgz
	sudo cp cockroach-v20.2.5.linux-amd64/cockroach /usr/local/bin/
