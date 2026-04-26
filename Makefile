GOCACHE ?= $(CURDIR)/.gocache

.PHONY: fmt
fmt:
	gofmt -w cmd internal api/gen scripts

.PHONY: test
test:
	GOCACHE=$(GOCACHE) go test ./...

.PHONY: build
build:
	GOCACHE=$(GOCACHE) go build ./...

.PHONY: proto-gen
proto-gen:
	bash scripts/gen-proto.sh

.PHONY: all
all: proto-gen build test
