UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Linux)
  COCKROACH_BIN ?= https://binaries.cockroachdb.com/cockroach-v20.2.5.linux-amd64.tgz
  HELM_BIN ?= https://get.helm.sh/helm-v3.8.0-linux-amd64.tar.gz
  KIND_BIN ?= https://kind.sigs.k8s.io/dl/v0.11.1/kind-linux-amd64
  KUBECTL_BIN ?= https://dl.k8s.io/release/v1.23.3/bin/linux/amd64/kubectl
  YQ_BIN ?= https://github.com/mikefarah/yq/releases/download/v4.31.2/yq_linux_amd64
  JQ_BIN ?= https://github.com/stedolan/jq/releases/download/jq-1.6/jq-linux64
  OPM_TAR ?= https://mirror.openshift.com/pub/openshift-v4/x86_64/clients/ocp/latest-4.8/opm-linux-4.8.57.tar.gz
  OPM_BIN ?= opm
endif
ifeq ($(UNAME_S),Darwin)
  COCKROACH_BIN ?= https://binaries.cockroachdb.com/cockroach-v20.2.5.darwin-10.9-amd64.tgz
  HELM_BIN ?= https://get.helm.sh/helm-v3.8.0-darwin-amd64.tar.gz
  KIND_BIN ?= https://kind.sigs.k8s.io/dl/v0.11.1/kind-darwin-amd64
  KUBECTL_BIN ?= https://dl.k8s.io/release/v1.23.3/bin/darwin/amd64/kubectl
  YQ_BIN ?= https://github.com/mikefarah/yq/releases/download/v4.31.2/yq_darwin_amd64
  JQ_BIN ?= https://github.com/stedolan/jq/releases/download/jq-1.6/jq-osx-amd64
  OPM_TAR ?= https://mirror.openshift.com/pub/openshift-v4/x86_64/clients/ocp/latest-4.8/opm-mac-4.8.57.tar.gz
  OPM_BIN ?= darwin-amd64-opm
endif

KIND_CLUSTER ?= chart-testing
REPOSITORY ?= gcr.io/cockroachlabs-helm-charts/cockroach-self-signer-cert

export BUNDLE_IMAGE ?= cockroach-operator-bundle
export HELM_OPERATOR_IMAGE ?= cockroach-helm-operator
export OPERATOR_IMAGE ?= cockroach-operator
export QUAY_DOCKER_REGISTRY ?= quay.io
export QUAY_PROJECT ?= cockroachdb
export VERSION ?= $(shell cat version.txt)


.DEFAULT_GOAL := all
all: build

BOLD = \033[1m
CLEAR = \033[0m
CYAN = \033[36m

help: ## Display this help
	@awk '\
		BEGIN {FS = ":.*##"; printf "Usage: make $(CYAN)<target>$(CLEAR)\n"} \
		/^[a-z0-9]+([\/]%)?([\/](%-)?[a-z\-0-9%]+)*:.*? ##/ { printf "  $(CYAN)%-15s$(CLEAR) %s\n", $$1, $$2 } \
		/^##@/ { printf "\n$(BOLD)%s$(CLEAR)\n", substr($$0, 5) }' \
		$(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: build/chart build/self-signer ## build the helm chart and self-signer

build/chart: bin/helm ## build the helm chart to build/artifacts
	@build/make.sh

build/self-signer: bin/yq ## build the self-signer image
	@docker build --platform=linux/amd64 -f build/docker-image/self-signer-cert-utility/Dockerfile \
		--build-arg COCKROACH_VERSION=$(shell bin/yq '.appVersion' ./cockroachdb/Chart.yaml) \
		-t ${REPOSITORY}:$(shell bin/yq '.tls.selfSigner.image.tag' ./cockroachdb/values.yaml) .

##@ Release

release: ## publish the build artifacts to S3
	@build/release.sh

build-and-push/self-signer: bin/yq ## push the self-signer image
	@docker buildx build --platform=linux/amd64,linux/arm64 -f build/docker-image/self-signer-cert-utility/Dockerfile \
		--build-arg COCKROACH_VERSION=$(shell bin/yq '.appVersion' ./cockroachdb/Chart.yaml) --push \
		-t ${REPOSITORY}:$(shell bin/yq '.tls.selfSigner.image.tag' ./cockroachdb/values.yaml) .

##@ Dev
dev/clean: ## remove built artifacts
	@rm -r build/artifacts/

##@ Test

test/cluster: bin/kind ## start a local kind cluster for testing
	@bin/kind get clusters -q | grep $(KIND_CLUSTER) || bin/kind create cluster --name $(KIND_CLUSTER)

test/e2e/%: PKG=$*
test/e2e/%: bin/cockroach bin/kubectl bin/helm build/self-signer test/publish-images-to-kind ## run e2e tests for package (e.g. install or rotate)
	@PATH="$(PWD)/bin:${PATH}" go test -v ./tests/e2e/$(PKG)/...

test/lint: bin/helm ## lint the helm chart
	@build/lint.sh && bin/helm lint cockroachdb

IMAGE_LIST = cockroachdb/cockroach:v21.1.1 quay.io/jetstack/cert-manager-cainjector:v1.11.0 quay.io/jetstack/cert-manager-webhook:v1.11.0 quay.io/jetstack/cert-manager-controller:v1.11.0 quay.io/jetstack/cert-manager-ctl:v1.11.0
test/publish-images-to-kind: bin/yq test/cluster ## publish signer and cockroach image to local kind registry
	for i in $(IMAGE_LIST); do \
		docker pull $$i; \
		bin/kind load docker-image $$i --name $(KIND_CLUSTER); \
	done
	@bin/kind load docker-image \
		${REPOSITORY}:$(shell bin/yq '.tls.selfSigner.image.tag' ./cockroachdb/values.yaml) \
		--name $(KIND_CLUSTER)

test/template: bin/cockroach bin/helm ## Run template tests
	@PATH="$(PWD)/bin:${PATH}" go test -v ./tests/template/...

test/units: bin/cockroach ## Run unit tests in ./pkg/...
	@PATH="$(PWD)/bin:${PATH}" go test -v ./pkg/...

##@ Binaries
bin: bin/cockroach bin/helm bin/kind bin/kubectl bin/yq ## install all binaries

bin/cockroach: ## install cockroach
	@mkdir -p bin
	@curl -L $(COCKROACH_BIN) | tar -xzf - -C bin/ --strip-components 1
	@rm -rf bin/lib

bin/helm: ## install helm
	@mkdir -p bin
	@curl -L $(HELM_BIN) | tar -xzf - -C bin/ --strip-components 1
	@rm -f bin/README.md bin/LICENSE

bin/kind: ## install kind
	@mkdir -p bin
	@curl -Lo bin/kind $(KIND_BIN)	
	@chmod +x bin/kind

bin/kubectl: ## install kubectl
	@mkdir -p bin
	@curl -Lo bin/kubectl $(KUBECTL_BIN)
	@chmod +x bin/kubectl

bin/yq: ## install yq
	@mkdir -p bin
	@curl -Lo bin/yq $(YQ_BIN)
	@chmod +x bin/yq

bin/jq: ## install jq
	@mkdir -p bin
	@curl -Lo bin/jq $(JQ_BIN)
	@chmod +x bin/jq

bin/opm: ## install opm
	@mkdir -p bin
	@curl -Lo bin/opm.tar.gz $(OPM_TAR)
	@tar xvf bin/opm.tar.gz
	@mv $(OPM_BIN) bin/opm
	@chmod +x bin/opm

build-and-release-olm-operator: bin/yq bin/jq bin/opm
	./build/olm_builder.sh

prepare_bundle: bin/yq bin/jq
	./build/olm_builder.sh "update_olm_operator"

build-and-push-operator-image:
	docker buildx build --platform=linux/amd64,linux/arm64 \
		-t $(QUAY_DOCKER_REGISTRY)/$(QUAY_PROJECT)/$(HELM_OPERATOR_IMAGE):$(VERSION) --push -f build/docker-image/operator/Dockerfile .

build-and-push-bundle-image:
	docker buildx build --platform=linux/amd64,linux/arm64 \
		-t $(QUAY_DOCKER_REGISTRY)/$(QUAY_PROJECT)/$(BUNDLE_IMAGE):$(VERSION) --push -f build/docker-image/olm-catalog/bundle.Dockerfile ./
