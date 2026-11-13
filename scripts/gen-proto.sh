#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
bin=$(mktemp -d)
trap 'rm -rf "$bin"' EXIT
GOBIN=$bin go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
GOBIN=$bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
PATH=$bin:$PATH protoc -I api --go_out=api --go_opt=paths=source_relative \
  --go-grpc_out=api --go-grpc_opt=paths=source_relative api/acplug/v1/*.proto
