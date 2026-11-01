# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Claude Code statusline binary (Go, single `main` package) that shows LiteLLM budget usage, plus two thin editor status-bar widgets (VS Code / IntelliJ) that shell out to the same binary with `--json`.

## Commands

Toolchains (Go, golangci-lint, Node, Java) are pinned in `mise.toml`; run `mise install` first.

```bash
mise run build            # go build -o claude-code-litellm-plugin .
mise run test             # go test -v -count=1 ./...  (also rewrites examples.svg)
mise run lint             # go fmt + go mod tidy + golangci-lint run
mise run vscode:build     # npm install/compile/package -> .vsix
mise run intellij:build   # ./gradlew buildPlugin -> zip
mise run build:all

go test -run TestFormatStatusLine -v ./...   # single test
echo '{"model":{"display_name":"Opus"}}' | go run . [--json]   # run locally (reads Claude Code's stdin JSON)
```

Pre-commit runs fmt, tidy, lint and test via mise. CI fails if `gofmt -l` is non-empty, `go mod tidy` changes files, or the `go` line in `go.mod` differs from the `go` version in `mise.toml` — bump both together. golangci-lint must be built with a Go >= the `go.mod` version.

`go.mod` sets `godebug default=go1.26` deliberately (keeps `SSL_CERT_FILE`/`SSL_CERT_DIR` from overriding the macOS/Windows system cert store); don't remove it when upgrading Go.

Releases: `./release.sh --type=patch|minor|major` tags and creates a GitHub release; the Release workflow then runs GoReleaser (injects `main.Version` via ldflags) and builds/attaches the .vsix and IntelliJ zip with the version stamped from the tag.

## Architecture

All Go logic lives in `main.go`; tests are `main_test.go` (unit + `httptest` mock-server tests) and `examples_test.go` (`TestGenerateExamples` renders sample lines into `examples.svg`, which is committed and shown in the README — expect it to change on every test run).

Flow in `main()`:
1. `readStatusInput` parses Claude Code's stdin JSON (model display name, context-window usage).
2. `resolveCredential` picks token + base URL: `LITELLM_PROXY_API_KEY` → `ANTHROPIC_AUTH_TOKEN` → the LiteLLM gateway SSO token (`enterpriseGateway`) from Claude Code's credential store (`~/.claude/.credentials.json`, then macOS Keychain via `/usr/bin/security`; `LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE` overrides and disables Keychain). The gateway token is bound to its issuing gateway: only sent if the configured base URL matches its origin, over HTTPS (loopback excepted), and if not expired. `claudeAiOauth` is intentionally never used. Preserve these security invariants when touching credential code.
3. `budgetInfo` — API-key mode uses `/key/info` then `enrichTeamBudget` (`/team/info`); gateway mode uses `/user/info` (gateway tokens aren't DB key rows) and falls back to user-level budget when the team is unbudgeted. Only the team/effective budget is displayed, never key-level spend.
4. Caching under `$XDG_CACHE_HOME` (or `~/.cache`), keyed by a hash of the credential: 30s success cache, 10s negative cache that replays the prior error, 1h GitHub latest-release cache for the update indicator. Writes are atomic (temp file + rename). Tests set `XDG_CACHE_HOME` to `t.TempDir()`.
5. Output: `renderLine` (ANSI status line) or `--json` → `buildStatusJSON`/`StatusJSON`. HTTP timeout is 3s because the binary runs as a frequently-invoked subprocess.

Editor plugins (`vscode-extension/src/extension.ts`, `intellij-plugin/.../LitellmWidget.kt`) contain no budget logic: they resolve the binary path (`~/.local/bin`, etc., falling back to PATH), merge the `env` block of `~/.claude/settings.json` into the child env (GUI apps don't inherit shell env), poll `--json` on an interval, and render. Changes to the `StatusJSON` fields must be mirrored in both plugins.

User-facing env vars beyond auth: `LITELLM_PLUGIN_SHOW_COST`, `LITELLM_PLUGIN_PREFIX`. Keep the README's env-var/troubleshooting sections in sync when behavior changes.
