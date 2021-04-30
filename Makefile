.DEFAULT_GOAL := all
all: build

.PHONY: build
build:
	build/make.sh


.PHONY: test
test:
	go test ./test

.PHONY: lint
lint:
	build/lint.sh

.PHONY: release
release:
	build/release.sh

.PHONY: clean
clean:
	rm -r build/artifacts/
