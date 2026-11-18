#!/usr/bin/env bash
# Runs locally what .github/workflows/ci.yml ran on pull requests: build, vet,
# race-enabled tests, golangci-lint and the ts/edgeauth checks. GitHub Actions
# CI is off, so this is the only check a change gets. Stops at the first failure and exits non-zero.
#
# Left out: pr-title.yml checked that the PR title is a conventional commit.
# A title only exists once the PR does, so no local step can check it; write
# it as one (feat:, fix:, chore: ...), because release-please reads it.
set -euo pipefail
cd "$(dirname "$0")/.."

GOLANGCI_LINT_VERSION=v2.11.3 # the version ci.yml pinned

step() { printf '\n==> %s\n' "$*"; }
need() { # <tool> <install hint>
  command -v "$1" >/dev/null || { echo "check.sh: $1 is not installed. $2" >&2; exit 1; }
}

need go "Install Go: https://go.dev/dl/ (or brew install go)"
need gcc "Install a C compiler (sudo apt install build-essential, or xcode-select --install); go test -race needs cgo"
# CI ran the Go version go.mod names (setup-go go-version-file). A newer local
# Go writes export data golangci-lint $GOLANGCI_LINT_VERSION cannot read, so
# run that version here too; go downloads it once into the module cache.
export GOTOOLCHAIN=$(awk '$1=="toolchain"{t=$2} $1=="go"{g="go"$2} END{print t?t:g}' go.mod)

step "go build, vet and test -race (needs ports 8080, 8081 and 9464 free)"
go build ./...
go vet ./...
go test -race ./...

step "golangci-lint $GOLANGCI_LINT_VERSION"
go run "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$GOLANGCI_LINT_VERSION" run --allow-parallel-runners

step "ts/edgeauth: deps, types, committed dist, tests under Node and Bun"
need npm "Install Node 22 or newer: https://nodejs.org (or brew install node)"
need bun "Install Bun: https://bun.sh (or brew install oven-sh/bun/bun)"
(
  cd ts/edgeauth
  npm ci --no-audit --no-fund
  npm run check
  npm run typecheck
  npm run build
  git diff --exit-code -- dist || { echo "check.sh: ts/edgeauth/dist is stale: run npm run build there and commit it" >&2; exit 1; }
  npm test
  bun --bun x vitest run
)

printf '\ncheck.sh: all checks passed\n'
