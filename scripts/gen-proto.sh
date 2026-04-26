#!/usr/bin/env bash
# scripts/gen-proto.sh — regenerate Go code from proto files.
#
# Requirements:
#   protoc           (brew install protobuf)
#   protoc-gen-go    (go install google.golang.org/protobuf/cmd/protoc-gen-go@latest)
#   protoc-gen-go-grpc (go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$SCRIPT_DIR/.."
PROTO_ROOT="$ROOT/api/proto"
OUT_DIR="$ROOT/api/gen"

mkdir -p "$OUT_DIR"

# Ensure protoc plugins are on PATH
export PATH="$PATH:$(go env GOPATH)/bin"

protoc \
  --proto_path="$PROTO_ROOT" \
  --go_out="$OUT_DIR" \
  --go_opt=paths=source_relative \
  --go-grpc_out="$OUT_DIR" \
  --go-grpc_opt=paths=source_relative \
  "$PROTO_ROOT/yadcc/v1/common.proto" \
  "$PROTO_ROOT/yadcc/v1/scheduler.proto" \
  "$PROTO_ROOT/yadcc/v1/daemon.proto" \
  "$PROTO_ROOT/yadcc/v1/cache.proto"

echo "proto generation done -> $OUT_DIR"
