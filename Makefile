.PHONY: all build test test-integration migrate fmt vet check

all: check

build:
	go build ./...

test:
	go test ./...

test-integration:
	AUTODEPLOY_TEST_DATABASE_URL="$${AUTODEPLOY_TEST_DATABASE_URL:?set AUTODEPLOY_TEST_DATABASE_URL}" go test -tags=integration ./internal/store/postgres ./migrations

migrate:
	go run ./cmd/migrate

fmt:
	test -z "$$(gofmt -l .)"

vet:
	go vet ./...

check: fmt vet test build
