GOCACHE ?= $(CURDIR)/.gocache
BIN     ?= $(CURDIR)/bin

.PHONY: fmt
fmt:
	gofmt -w cmd internal api/gen scripts

.PHONY: test
test:
	GOCACHE=$(GOCACHE) go test ./...

.PHONY: build
build:
	GOCACHE=$(GOCACHE) go build -o $(BIN)/yadcc           ./cmd/yadcc/
	GOCACHE=$(GOCACHE) go build -o $(BIN)/yadcc-daemon    ./cmd/yadcc-daemon/
	GOCACHE=$(GOCACHE) go build -o $(BIN)/yadcc-scheduler ./cmd/yadcc-scheduler/
	GOCACHE=$(GOCACHE) go build -o $(BIN)/yadcc-cache     ./cmd/yadcc-cache/

.PHONY: proto-gen
proto-gen:
	bash scripts/gen-proto.sh

.PHONY: all
all: proto-gen build test
