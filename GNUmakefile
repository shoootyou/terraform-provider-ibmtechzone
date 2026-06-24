default: build

# Go binary output name
BINARY := terraform-provider-ibmtechzone

# Build the provider binary for the local platform.
build:
	go build -o $(BINARY) .

# Run unit and framework-level tests (no TF_ACC — no live API calls).
# Acceptance tests are opt-in via `make testacc` (they land in E5).
test:
	go test ./... -count=1

# Run acceptance tests against the mock TechZone server.
# Requires TF_ACC=1 to be set; acceptance tests silently skip without it.
# The live-API variant is gated behind a separate build tag / env flag and
# never runs in CI by default (it requires a human-issued TechZone token and
# a live pool account).
testacc:
	TF_ACC=1 go test ./... -count=1 -timeout 120m

# Run golangci-lint (must be installed separately; CI pins the version via action).
lint:
	golangci-lint run ./...

# Run go generate to regenerate any generated code / documentation.
generate:
	go generate ./...

.PHONY: build test testacc lint generate
