# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Claude Code statusline binary (Go, single `main` package) that shows LiteLLM budget usage, plus two thin editor status-bar widgets (VS Code / IntelliJ) that shell out to the same binary with `--json`. The binary also carries its own installer logic (`setup`) and diagnostics (`doctor`).

## Commands

Toolchains (Go, golangci-lint, GoReleaser, Node, Java) are pinned in `mise.toml`; run `mise install` first.

```bash
mise run build            # go build with -X main.Version=$(git describe)
mise run test             # go test -v -count=1 ./...  (also rewrites examples.svg)
mise run lint             # go fmt + go mod tidy + golangci-lint run
mise run install:local    # build, copy to ~/.local/bin, run setup
mise run vscode:build     # npm ci/compile/package -> .vsix
mise run intellij:build   # ./gradlew buildPlugin -> zip (JDK 21 from mise, emits Java 17 bytecode)
mise run intellij:run     # sandbox IDE with the plugin
mise run release:snapshot # full release dry run into dist/ (binaries, plugins, checksums, brew/scoop)

go test -run TestFormatStatusLine -v ./...   # single test
echo '{"model":{"display_name":"Opus"}}' | go run . [--json]   # run locally (reads Claude Code's stdin JSON)
go run . doctor
```

Pre-commit runs fmt, tidy, lint and test via mise. CI fails if `gofmt -l` is non-empty, `go mod tidy` changes files, `examples.svg` differs after tests, shellcheck fails on the scripts, or the `go` line in `go.mod` differs from the `go` version in `mise.toml` — bump both together. golangci-lint must be built with a Go >= the `go.mod` version. CI also runs the tests on macOS/Windows and a `goreleaser` snapshot release.

`go.mod` sets `godebug default=go1.26` deliberately (keeps `SSL_CERT_FILE`/`SSL_CERT_DIR` from overriding the macOS/Windows system cert store); don't remove it when upgrading Go.

Releases: `./release.sh --type=patch|minor|major [--pre=rc.N]` checks the repo (clean, on main, synced, CI green) and pushes a tag. `.github/workflows/release.yml` then runs `go test`, `mise run release:artifacts <tag>` (version-stamped .vsix + IntelliJ zip; fails if the names lack the version) and `goreleaser release`. GoReleaser creates the GitHub release as a draft, uploads everything (checksums cover the .vsix/zip — `setup --editors` relies on that), publishes it, then pushes the Homebrew cask / Scoop manifest (skipped without the `HOMEBREW_TAP_TOKEN` secret, and for prereleases).

## Architecture

Go files: `main.go` (status line: credentials, fetch, cache, render, `run()` dispatch), `setup.go` (`setup` subcommand: order-preserving settings.json merge, command-path choice, `--editors`), `doctor.go` (`doctor` subcommand). Tests: `main_test.go`, `core_test.go`, `setup_test.go`, `doctor_test.go` (unit + `httptest` mock servers), and `examples_test.go` (`TestGenerateExamples` renders sample lines into `examples.svg`, committed and shown in the README; output is deterministic, so a diff means rendering changed).

Flow in `run()` (no args = status line; `--json`; `setup`; `doctor`; `--version`; `--help`):
1. `readStatusInput` parses Claude Code's stdin JSON (model display name, context-window usage). Skipped when stdin is a terminal (a human ran it), so it never hangs.
2. `--json` (editor mode) first applies the `env` block of `<config dir>/settings.json` (`applyClaudeSettingsEnv`, overriding the process env like Claude Code does). Plain status-line mode must never do this: Claude Code already applied it, including project-level overrides.
3. `resolveCredential` picks token + base URL: `LITELLM_PROXY_API_KEY` → `ANTHROPIC_AUTH_TOKEN` → the LiteLLM gateway SSO token (`enterpriseGateway`) from Claude Code's credential store (`<config dir>/.credentials.json`, then macOS Keychain via `/usr/bin/security`; `LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE` overrides and disables Keychain; a custom `CLAUDE_CONFIG_DIR` also disables it because Claude Code keys an undocumented, different Keychain item per config dir). The gateway token is bound to its issuing gateway: only sent if the configured base URL matches its origin, over HTTPS (loopback excepted), and if not expired. `claudeAiOauth` is intentionally never used. Preserve these security invariants when touching credential code. `resolveCredentialWithNotes` returns the same result plus non-secret notes for `doctor`.
4. `budgetInfo` (cached) → `fetchBudget` (uncached, also used by `doctor`), bounded by one 4s `BudgetDeadline` context. All HTTP goes through `getJSON` (401/403 → `ErrAuth`, LiteLLM `budget_exceeded` → `*BudgetExceededError`, other non-200 → `*httpStatusError`). API-key mode uses `/key/info` then `enrichTeamBudget` (`/team/info`; 401/403/404 mean "no team budget", other failures propagate); gateway mode uses `/user/info` (gateway tokens aren't DB key rows) and falls back to the user-level budget when the team is unbudgeted. Only the team/effective budget is displayed, never key-level spend. No base URL → `ErrNoBaseURL` without any request.
5. The GitHub latest-release check runs concurrently with the budget fetch, writes its 1h backoff entry *before* the request (so a killed process still backs off), and is off for `dev` builds or `LITELLM_PLUGIN_UPDATE_CHECK=0`.
6. Caching under `$XDG_CACHE_HOME` (or `~/.cache`), keyed by a hash of the credential: 30s success cache, 10s negative cache that replays the prior error. Cache writes go through `writeCacheFile` (atomic temp file + rename, sweeps orphaned temps in the cache dir only). Tests set `XDG_CACHE_HOME` to `t.TempDir()`.
7. Output: `renderLine` (ANSI status line) or `--json` → `buildStatusJSON`/`StatusJSON`; both classify errors via `classifyError`, so the two modes can't drift (`TestOutputModeParity`).

`setup` never overwrites invalid JSON, keeps top-level key order, writes no BOM, follows symlinks, leaves a foreign `statusLine` alone (exit 2) unless `--force`, and writes the command as a stable path (`chooseCommand`: PATH symlink if it's this binary, bare name on Windows, else `~/…` path). The install scripts (`install.sh`, `install.ps1`) only download + verify checksums + place the binary, then delegate to `setup` and `doctor` — keep settings logic in Go, not in the scripts.

Editor plugins (`vscode-extension/src/extension.ts`, `intellij-plugin/.../LitellmWidget.kt`) contain no budget or settings logic: they resolve the binary (same candidate list in both: installer, Homebrew, Scoop locations, then PATH), run `--json` with a 10s timeout, and render. Both show the same states (not found / timeout / error / outdated binary when `version` is missing from the JSON) and the same tooltip content. Changes to the `StatusJSON` fields or these behaviors must be mirrored in both plugins.

User-facing env vars beyond auth: `LITELLM_PLUGIN_SHOW_COST`, `LITELLM_PLUGIN_PREFIX`, `LITELLM_PLUGIN_UPDATE_CHECK`; installer: `LITELLM_PLUGIN_EDITORS`, `LITELLM_PLUGIN_VERSION`, `LITELLM_PLUGIN_INSTALL_DIR`, `LITELLM_PLUGIN_NO_MODIFY_PATH`. Keep the README's env-var/troubleshooting sections in sync when behavior changes.
