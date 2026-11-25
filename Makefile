GOFUMPT_VERSION := v0.12.0
MODERNIZE_VERSION := v0.53.0
GOIMPORTS_REREVISER_VERSION := v4.2.0
GOLANGCI_LINT_VERSION := v2.14.0

# Space-separated, repository-relative package directories, including descendants.
FMT_PATHS ?= .
override shell_quote = '$(subst ','"'"',$(1))'
override fmt_dirs = $(or $(strip $(FMT_PATHS)),.)
override fmt_paths = $(foreach dir,$(fmt_dirs),$(call shell_quote,$(dir)))
override fmt_packages = $(foreach dir,$(fmt_dirs),$(call shell_quote,$(if $(filter . ./,$(dir)),./...,./$(patsubst ./%,%,$(patsubst %/,%,$(dir)))/...)))
override imports_flags := -project-name=github.com/zchee/claude-code-model-gateway -use-cache -cache-fast-skip -rm-unused -set-alias -recursive

# Preserve trailing newlines in directory names when capturing pwd output.
override validate_fmt_paths = root=$$(pwd -P && printf .) || exit 1; root=$${root%??}; \
	for dir in $(fmt_paths); do \
		case "/$$dir/" in \
			/-*|//*|*/../*|*...*) \
				printf 'invalid FMT_PATHS entry: %s (must not start with - or / or contain a .. component or ...)\n' "$$dir" >&2; exit 1 ;; \
		esac; \
		if ! resolved=$$(cd -P "./$$dir" 2>/dev/null && pwd -P && printf .); then \
			printf 'invalid FMT_PATHS entry: %s (not an accessible directory)\n' "$$dir" >&2; exit 1; \
		fi; \
		case "$${resolved%??}" in \
			"$$root"|"$${root%/}"/*) ;; \
			*) printf 'invalid FMT_PATHS entry: %s (resolves outside the checkout)\n' "$$dir" >&2; exit 1 ;; \
		esac; \
	done

# Keep the guard and the protected command in one shell, even with make -i or -k.
override require_test_venue = if [ -L hack ] || [ -L hack/venue-check ]; then \
		printf '%s\n' 'not a test venue: hack and hack/venue-check must not be symbolic links' >&2; exit 1; \
	elif [ ! -f hack/venue-check ] || [ ! -x hack/venue-check ]; then \
		printf '%s\n' 'not a test venue: hack/venue-check is missing or not executable' >&2; exit 1; \
	elif ! ./hack/venue-check; then \
		printf '%s\n' 'not a test venue: hack/venue-check failed' >&2; exit 1; \
	fi

override run_script = if [ -f "hack/$@" ] && [ -x "hack/$@" ]; then \
		"./hack/$@"; \
	else \
		printf 'hack/%s is missing or not executable\n' "$@" >&2; exit 1; \
	fi

TRIPWIRE_NAME ?= phase-0-baseline
unexport TRIPWIRE_NAME

.PHONY: build vet build-windows fmt fmt-check lint test test-process conformance baseline-node assertion-sites bench tripwire tools

build:
	go build ./...

vet:
	go vet ./...

build-windows:
	GOOS=windows GOARCH=amd64 go build ./...

# A nonempty GOFLAGS overrides Go's saved defaults. Keep module metadata unchanged.
override fmt_environment := GOFLAGS=-mod=readonly GOWORK=off
# modernize's -tags flag is a no-op; GOFLAGS passes tags to go list.
override fmt_process_flags := GOFLAGS='-mod=readonly -tags=process'

fmt:
	@$(validate_fmt_paths); \
	export $(fmt_environment); \
	gofmt -s -w -- $(fmt_paths) && \
	gofumpt -w -extra -- $(fmt_paths) && \
	modernize -fix -test -- $(fmt_packages) && \
	$(fmt_process_flags) modernize -fix -test -- $(fmt_packages) && \
	goimports-rereviser $(imports_flags) -- $(fmt_paths)

fmt-check:
	@$(validate_fmt_paths); \
	export $(fmt_environment); \
	status=0; \
	check() { \
		output=$$("$$@") || status=1; \
		if [ -n "$$output" ]; then printf '%s\n' "$$output"; status=1; fi; \
	}; \
	check gofmt -s -l -- $(fmt_paths); \
	check gofumpt -l -extra -- $(fmt_paths); \
	check modernize -fix -diff -test -- $(fmt_packages); \
	check env $(fmt_process_flags) modernize -fix -diff -test -- $(fmt_packages); \
	check goimports-rereviser $(imports_flags) -list-diff -- $(fmt_paths); \
	exit "$$status"

lint:
	./hack/lint-venue
	./hack/parity-check structure
	golangci-lint run
	golangci-lint run --build-tags process

test:
	@$(require_test_venue) && go test -count=1 ./...

test-process:
	@$(require_test_venue) && go test -count=1 -tags process ./...

conformance baseline-node assertion-sites bench:
	@$(require_test_venue) && $(run_script)

tripwire:
	@./hack/tripwire compare $(call shell_quote,$(value TRIPWIRE_NAME))

tools:
	go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	go install golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize@$(MODERNIZE_VERSION)
	go install github.com/zchee/goimports-rereviser/v4@$(GOIMPORTS_REREVISER_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
