COCKROACH_BIN ?= https://binaries.cockroachdb.com/cockroach-v20.2.5.linux-amd64.tgz
HELM_BIN ?= https://get.helm.sh/helm-v3.8.0-linux-amd64.tar.gz
KIND_BIN ?= https://kind.sigs.k8s.io/dl/v0.11.1/kind-linux-amd64
KUBECTL_BIN ?= https://dl.k8s.io/release/v1.23.3/bin/linux/amd64/kubectl
YQ_BIN ?= https://github.com/mikefarah/yq/releases/download/2.2.1/yq_linux_amd64

KIND_CLUSTER ?= chart-testing
REPOSITORY ?= gcr.io/cockroachlabs-helm-charts/cockroach-self-signer-cert

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
	@docker build \
		-f build/docker-image/Dockerfile \
		-t ${REPOSITORY}:$(shell bin/yq r ./cockroachdb/values.yaml 'tls.selfSigner.image.tag') .

##@ Release

release: ## publish the build artifacts to S3
	@build/release.sh

push/self-signer: bin/yq ## push the self-signer image
	@docker push ${REPOSITORY}:$(shell bin/yq r ./cockroachdb/values.yaml 'tls.selfSigner.image.tag')

##@ Dev
dev/clean: ## remove built artifacts
	@rm -r build/artifacts/

##@ Test

test/cluster: bin/kind ## start a local kind cluster for testing
	@bin/kind get clusters -q | grep $(KIND_CLUSTER) || bin/kind create cluster --name $(KIND_CLUSTER)

test/e2e/%: PKG=$*
test/e2e/%: bin/cockroach bin/kubectl build/self-signer test/publish-images-to-kind ## run e2e tests for package (e.g. install or rotate)
	@PATH="$(PWD)/bin:${PATH}" go test -v ./tests/e2e/$(PKG)/...

test/lint: bin/helm ## lint the helm chart
	@build/lint.sh && bin/helm lint cockroachdb

test/publish-images-to-kind: bin/yq test/cluster ## publish signer and cockroach image to local kind registry
	@docker pull cockroachdb/cockroach:v21.1.1
	@bin/kind load docker-image cockroachdb/cockroach:v21.1.1 --name $(KIND_CLUSTER)
	@bin/kind load docker-image \
		${REPOSITORY}:$(shell bin/yq r ./cockroachdb/values.yaml 'tls.selfSigner.image.tag') \
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
