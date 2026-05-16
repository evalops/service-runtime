BAZEL ?= $(shell if command -v bazelisk >/dev/null 2>&1; then command -v bazelisk; elif command -v go >/dev/null 2>&1; then printf '%s/bin/bazelisk' "$$(go env GOPATH)"; else printf bazelisk; fi)
BUILDIFIER ?= $(shell if command -v buildifier >/dev/null 2>&1; then command -v buildifier; elif command -v go >/dev/null 2>&1; then printf '%s/bin/buildifier' "$$(go env GOPATH)"; else printf buildifier; fi)
BAZEL_TARGETS ?= //...
BAZEL_REMOTE_CONFIG ?= remote-gcp-dev
BAZEL_RBE_SMOKE_TARGETS ?= //async:async_test //health:health_test //resilience:resilience_test
BAZEL_CI_REMOTE_DOWNLOAD_FLAGS ?= --remote_download_outputs=minimal

.PHONY: lint test install-hooks bazel-mod-tidy bazel-gazelle bazel-format bazel-check bazel-test bazel-test-remote bazel-rbe-smoke

lint:
	golangci-lint run ./...

test:
	go test -race ./...

install-hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Pre-commit hook installed"

bazel-mod-tidy:
	$(BAZEL) mod tidy

bazel-gazelle:
	$(BAZEL) run //:gazelle

bazel-format:
	$(BUILDIFIER) -r .

bazel-check:
	$(MAKE) bazel-mod-tidy
	$(MAKE) bazel-gazelle
	$(MAKE) bazel-format
	git diff --exit-code

bazel-test:
	$(BAZEL) test $(BAZEL_TARGETS)

bazel-test-remote:
	$(BAZEL) test --config=$(BAZEL_REMOTE_CONFIG) $(BAZEL_TARGETS)

bazel-rbe-smoke:
	scripts/run-bazel-rbe.sh -- $(BAZEL) test --config=$(BAZEL_REMOTE_CONFIG) $(BAZEL_CI_REMOTE_DOWNLOAD_FLAGS) $(BAZEL_RBE_SMOKE_TARGETS)
