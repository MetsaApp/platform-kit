#!/usr/bin/env bash
# Runs locally what .github/workflows/ci.yml ran on pull requests: build, vet,
# race-enabled tests and golangci-lint. GitHub Actions CI is off, so this is
# the only check a change gets. Stops at the first failure and exits non-zero.
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

printf '\ncheck.sh: all checks passed\n'
