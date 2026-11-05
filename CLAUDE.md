# platform-kit

See README.md for what this module is and how to work here.

## Local validation

GitHub Actions CI is off to save paid minutes, so nothing checks a pull request on GitHub. Run `scripts/check.sh` and make it pass before you push. It runs what CI ran: `go build`, `go vet`, `go test -race` and golangci-lint v2.11.3, on the Go version in go.mod. The tests start a service on ports 8080, 8081 and 9464, so those must be free. There is no `--full` mode. The pull request title must be a conventional commit (`feat:`, `fix:`, `chore:` ...) because release-please reads it, and nothing checks it now.

The pre-push hook in `.githooks/pre-push` runs `scripts/check.sh` before every push, and `git push --no-verify` skips it. Claude Code sessions turn the hook on through `.claude/settings.json`; people turn it on once per clone with `git config core.hooksPath .githooks`.
