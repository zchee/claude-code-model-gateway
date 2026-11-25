GOFUMPT_VERSION := v0.12.0
MODERNIZE_VERSION := v0.53.0
GOIMPORTS_REREVISER_VERSION := v4.2.0
GOLANGCI_LINT_VERSION := v2.14.0

# Space-separated, repository-relative package directories, including descendants.
FMT_PATHS ?= .
fmt_paths = $(or $(strip $(FMT_PATHS)),.)
fmt_packages = $(foreach dir,$(fmt_paths),$(if $(filter . ./,$(dir)),./...,./$(patsubst ./%,%,$(patsubst %/,%,$(dir)))/...))
imports_flags := -project-name=github.com/zchee/claude-code-model-gateway -use-cache -cache-fast-skip -rm-unused -set-alias -recursive

# Keep the guard and the protected command in one shell, even with make -i or -k.
require_test_venue = if [ ! -f hack/venue-check ] || [ ! -x hack/venue-check ]; then \
		printf '%s\n' 'not a test venue: hack/venue-check is missing or not executable' >&2; exit 1; \
	elif ! ./hack/venue-check; then \
		printf '%s\n' 'not a test venue: hack/venue-check failed' >&2; exit 1; \
	fi

run_script = if [ -f "hack/$@" ] && [ -x "hack/$@" ]; then \
		"./hack/$@"; \
	else \
		printf 'hack/%s is missing or not executable\n' "$@" >&2; exit 1; \
	fi

.PHONY: build vet build-windows fmt fmt-check lint test test-process conformance bench tripwire tools

build:
	go build ./...

vet:
	go vet ./...

build-windows:
	GOOS=windows GOARCH=amd64 go build ./...

fmt:
	gofmt -s -w $(fmt_paths) && \
	gofumpt -w -extra $(fmt_paths) && \
	modernize -fix -test $(fmt_packages) && \
	goimports-rereviser $(imports_flags) $(fmt_paths)

fmt-check:
	@status=0; \
	check() { \
		output=$$("$$@") || status=1; \
		if [ -n "$$output" ]; then printf '%s\n' "$$output"; status=1; fi; \
	}; \
	check gofmt -s -l $(fmt_paths); \
	check gofumpt -l -extra $(fmt_paths); \
	check modernize -fix -diff -test $(fmt_packages); \
	check goimports-rereviser $(imports_flags) -list-diff $(fmt_paths); \
	exit "$$status"

lint:
	golangci-lint run
	golangci-lint run --build-tags process

test:
	@$(require_test_venue) && go test -count=1 ./...

test-process:
	@$(require_test_venue) && go test -count=1 -tags process ./...

conformance bench:
	@$(require_test_venue) && $(run_script)

tripwire:
	@$(run_script)

tools:
	go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	go install golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize@$(MODERNIZE_VERSION)
	go install github.com/zchee/goimports-rereviser/v4@$(GOIMPORTS_REREVISER_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
