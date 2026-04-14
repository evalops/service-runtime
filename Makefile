.PHONY: lint test install-hooks

lint:
	golangci-lint run ./...

test:
	go test -race ./...

install-hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Pre-commit hook installed"
