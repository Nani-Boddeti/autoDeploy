.PHONY: all build test fmt vet check

all: check

build:
	go build ./...

test:
	go test ./...

fmt:
	test -z "$$(gofmt -l .)"

vet:
	go vet ./...

check: fmt vet test build

