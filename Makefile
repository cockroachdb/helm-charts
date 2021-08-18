REPOSITORY ?= gcr.io/cockroachlabs-helm-charts/cockroach-self-signer-cert

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

get-tag: install-yq
	yq r ./cockroachdb/values.yaml 'tls.selfSigner.image.tag'

build-self-signer: install-yq
	$(eval TAG=$(shell yq r ./cockroachdb/values.yaml 'tls.selfSigner.image.tag'))
	docker build -f build/docker-image/Dockerfile -t ${REPOSITORY}:${TAG} .

push-self-signer:
	$(eval TAG=$(shell yq r ./cockroachdb/values.yaml 'tls.selfSigner.image.tag'))
	docker push ${REPOSITORY}:${TAG}

install-yq:
	curl -Lo yq https://github.com/mikefarah/yq/releases/download/2.2.1/yq_linux_amd64 && \
	chmod +x yq && sudo mv yq /usr/bin/

install-cockroach:
	sudo apt-get install wget -y
	wget https://binaries.cockroachdb.com/cockroach-v20.2.5.linux-amd64.tgz
	tar zxf cockroach-v20.2.5.linux-amd64.tgz
	sudo cp cockroach-v20.2.5.linux-amd64/cockroach /usr/local/bin/

load-docker-image-to-kind:
	$(eval TAG=$(shell yq r ./cockroachdb/values.yaml 'tls.selfSigner.image.tag'))
	wget https://kind.sigs.k8s.io/dl/v0.11.1/kind-linux-amd64
	sudo mv kind-linux-amd64 /usr/local/bin/kind
	sudo chmod +x /usr/local/bin/kind
	docker pull cockroachdb/cockroach:v21.1.1
	kind load docker-image ${REPOSITORY}:${TAG} --name chart-testing
	kind load docker-image cockroachdb/cockroach:v21.1.1 --name chart-testing
