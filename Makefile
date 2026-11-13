SHELL := /bin/bash
STATICCHECK_VERSION := 2026.2.2
BUF_VERSION := v1.50.0
BUF := go run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

.PHONY: fmt check test proto api-baseline conform

fmt:
	gofmt -w .

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	scripts/check-comments.sh
	scripts/check-deps.sh
	$(BUF) lint api
	$(BUF) breaking api --against api/baseline/acplug.binpb

test:
	go test -race -count=1 ./...

proto:
	scripts/gen-proto.sh

api-baseline:
	$(BUF) build api -o api/baseline/acplug.binpb

conform:
	go build -o bin/conform ./cmd/conform
