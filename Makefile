# Convenience targets for cline-vertex-gw development.
#
# Mostly a thin wrapper around `go` commands so contributors don't have to
# memorize the flag soup. CI uses the same incantations directly (see
# .github/workflows/ci.yml) so what passes locally with `make ci` matches.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
PKGS     := ./api/... ./provider/... .

.PHONY: help build run test race vet staticcheck vuln cover clean docker ci

help:
	@echo "Targets:"
	@echo "  build        Build the cline-vertex-gw binary (version=$(VERSION))"
	@echo "  run          Build and run (uses your env)"
	@echo "  test         Run unit tests with -count=1"
	@echo "  race         Run tests under the race detector"
	@echo "  vet          go vet ./..."
	@echo "  staticcheck  Run honnef.co/go/tools/cmd/staticcheck"
	@echo "  vuln         Run golang.org/x/vuln/cmd/govulncheck"
	@echo "  cover        Run tests + open HTML coverage in your browser"
	@echo "  docker       Build the distroless container image"
	@echo "  ci           Run the full local-CI gauntlet (vet + race + staticcheck + vuln)"

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o cline-vertex-gw .

run: build
	./cline-vertex-gw

test:
	go test -count=1 $(PKGS)

race:
	go test -race -count=1 $(PKGS)

vet:
	go vet $(PKGS)

staticcheck:
	@command -v staticcheck >/dev/null || go install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck $(PKGS)

vuln:
	@command -v govulncheck >/dev/null || go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck $(PKGS)

cover:
	go test -coverprofile=coverage.out $(PKGS)
	go tool cover -html=coverage.out

docker:
	docker build --build-arg VERSION=$(VERSION) -t cline-vertex-gw:$(VERSION) .

ci: vet race staticcheck vuln
	@echo "✅ All local CI checks passed."

clean:
	rm -f cline-vertex-gw coverage.out