REPOSITORY ?= gcr.io/cockroachlabs-helm-charts/cockroach-self-signer-cert
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

load-docker-image-to-kind:
	wget https://kind.sigs.k8s.io/dl/v0.11.1/kind-linux-amd64
	sudo mv kind-linux-amd64 /usr/local/bin/kind
	sudo chmod +x /usr/local/bin/kind
	docker pull ${REPOSITORY}:${TAG}
	docker pull cockroachdb/cockroach:v21.1.1
	kind load docker-image ${REPOSITORY}:${TAG} --name chart-testing
	kind load docker-image cockroachdb/cockroach:v21.1.1 --name chart-testing
