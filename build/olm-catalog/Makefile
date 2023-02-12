BUNDLE_IMAGE ?= cockroach-operator-bundle
COCKROACH_IMAGE ?= cockroachdb

build-cockroach-image:
	docker build -t hemanrnjn/$(COCKROACH_IMAGE):$(VERSION) -f Dockerfile ../..

push-cockroach-image:
	docker push hemanrnjn/$(COCKROACH_IMAGE):$(VERSION)

build-image:
	# docker build -t $(QUAY_DOCKER_REGISTRY)/$(QUAY_PROJECT)/$(BUNDLE_IMAGE):$(VERSION) -f bundle.Dockerfile ../..
	docker build -t hemanrnjn/$(BUNDLE_IMAGE):$(VERSION) -f bundle.Dockerfile ../..

build-push:
	# docker push $(QUAY_DOCKER_REGISTRY)/$(QUAY_PROJECT)/$(BUNDLE_IMAGE):$(VERSION)
	docker push hemanrnjn/$(BUNDLE_IMAGE):$(VERSION)

.PHONY: build-cockroach-image build-cockroach-push build-image build-push
include ../../Makefile