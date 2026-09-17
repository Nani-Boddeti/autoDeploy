.PHONY: all build test test-integration test-checkout-integration migrate fmt vet check

all: check

build:
	go build ./...

test:
	go test ./...

test-integration:
	AUTODEPLOY_TEST_DATABASE_URL="$${AUTODEPLOY_TEST_DATABASE_URL:?set AUTODEPLOY_TEST_DATABASE_URL}" go test -p 1 -tags=integration ./internal/store/postgres ./internal/agent/http ./migrations

# This runs against installed privileged helpers, never the normal test suite.
# It is deliberately Linux-only and requires an explicit operator opt-in.
test-checkout-integration:
	@test "$$(uname -s)" = Linux || { echo "checkout integration requires Linux"; exit 1; }
	@test "$${AUTODEPLOY_CHECKOUT_INTEGRATION:-}" = 1 || { echo "set AUTODEPLOY_CHECKOUT_INTEGRATION=1"; exit 1; }
	@: "$${AUTODEPLOY_CHECKOUT_QUOTA_SOCKET:?set AUTODEPLOY_CHECKOUT_QUOTA_SOCKET}"
	@: "$${AUTODEPLOY_CHECKOUT_SPAWN_SOCKET:?set AUTODEPLOY_CHECKOUT_SPAWN_SOCKET}"
	@: "$${AUTODEPLOY_CHECKOUT_CGROUP_ROOT:?set AUTODEPLOY_CHECKOUT_CGROUP_ROOT}"
	@: "$${AUTODEPLOY_CHECKOUT_AGENT_UID:?set AUTODEPLOY_CHECKOUT_AGENT_UID}"
	@: "$${AUTODEPLOY_CHECKOUT_AGENT_GID:?set AUTODEPLOY_CHECKOUT_AGENT_GID}"
	@: "$${AUTODEPLOY_CHECKOUT_UID:?set AUTODEPLOY_CHECKOUT_UID}"
	@: "$${AUTODEPLOY_CHECKOUT_GID:?set AUTODEPLOY_CHECKOUT_GID}"
	@: "$${AUTODEPLOY_CHECKOUT_QUOTA_BYTES:?set AUTODEPLOY_CHECKOUT_QUOTA_BYTES}"
	@: "$${AUTODEPLOY_CHECKOUT_QUOTA_INODES:?set AUTODEPLOY_CHECKOUT_QUOTA_INODES}"
	@: "$${AUTODEPLOY_CHECKOUT_TIMEOUT_SECONDS:?set AUTODEPLOY_CHECKOUT_TIMEOUT_SECONDS}"
	@: "$${AUTODEPLOY_CHECKOUT_MAX_OUTPUT:?set AUTODEPLOY_CHECKOUT_MAX_OUTPUT}"
	go test -count=1 -tags=checkout_integration ./internal/checkoutquota ./internal/checkoutcontain ./internal/gitcheckout

migrate:
	go run ./cmd/migrate

fmt:
	test -z "$$(gofmt -l .)"

vet:
	go vet ./...

check: fmt vet test build
