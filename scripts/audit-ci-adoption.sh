#!/usr/bin/env bash

set -euo pipefail

root_dir="${1:-$(pwd)}"

repos=(
  asb
  keys
  identity
  gate
  audit
  meter
  agent-mcp
  llm-gateway
  service-runtime
)

has_pattern() {
  local repo_dir="$1"
  local pattern="$2"

  if [ ! -d "$repo_dir/.github/workflows" ]; then
    return 1
  fi

  rg -q "$pattern" "$repo_dir/.github/workflows"
}

printf '| repo | setup-go-service | race | golangci-lint | gosec | govulncheck |\n'
printf '| --- | --- | --- | --- | --- | --- |\n'

for repo in "${repos[@]}"; do
  repo_dir="${root_dir%/}/${repo}"
  if [ ! -d "$repo_dir" ]; then
    printf '| %s | missing | missing | missing | missing | missing |\n' "$repo"
    continue
  fi

  setup='no'
  race='no'
  lint='no'
  gosec='no'
  govulncheck='no'

  if has_pattern "$repo_dir" 'setup-go-service'; then
    setup='yes'
  fi
  if has_pattern "$repo_dir" 'go-test-race:\s*"true"|go test .* -race|-race .*go test'; then
    race='yes'
  fi
  if has_pattern "$repo_dir" 'golangci-lint|run-golangci-lint:\s*"true"'; then
    lint='yes'
  fi
  if has_pattern "$repo_dir" '\bgosec\b|run-gosec:\s*"true"'; then
    gosec='yes'
  fi
  if has_pattern "$repo_dir" 'govulncheck|run-govulncheck:\s*"true"'; then
    govulncheck='yes'
  fi

  printf '| %s | %s | %s | %s | %s | %s |\n' \
    "$repo" "$setup" "$race" "$lint" "$gosec" "$govulncheck"
done
