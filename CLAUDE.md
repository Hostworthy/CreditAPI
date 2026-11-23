# CLAUDE.md

Guidance for AI assistants working on this repository. Human-facing docs:
[README.md](README.md), [docs/SETUP.md](docs/SETUP.md), [docs/DEV.md](docs/DEV.md).

## What This Is

A Go server that exposes an OpenAI-compatible API (`/v1/chat/completions`,
`/v1/models`) backed by the Claude Code CLI (`claude -p`, stream-json) on a
Claude subscription, plus `/admin` (virtual API keys) and `/login` (phone-friendly
`claude setup-token` sign-in). Any OpenAI-compatible app can be a client.

## Commands

```bash
make check      # tidy, gofmt, vet, tests: run before committing
make ci         # what CI runs: gofmt diff, vet, race tests
make e2e-ci     # Bruno end-to-end suite on the fake Claude CLI (needs Node)
make e2e        # server on the fake CLI for manual testing (.env.bruno)
```

## Layout

- `src/cmd/creditapi`: entrypoint. `src/cmd/fakeclaude`: fake CLI binary.
- `src/internal/{admin,claude,config,keys,logging,openai,server}`: application code.
- `src/tests`: integration tests on the full server; `src/tests/support`: helpers and the fake CLI.
- `bruno/`: Bruno collection (`.bru`), ordered folders, `environments/local.bru`.
- `docs/`: SETUP, DEV, API, ENVIRONMENT, SECURITY.

## Conventions

- Logging: `slog` with event-ID messages built by `logging.Event(op, action, status)`,
  e.g. `creditapi.chat.completion.succeeded`; details in attributes; use
  `logging.From(ctx)` inside handlers for the request ID. Never log tokens,
  keys, passwords, sign-in codes, prompts or responses.
- Errors to API clients use the OpenAI envelope (`type`, `code`, `message`);
  map new failure kinds in `openai.describeError` and document them in `docs/API.md`.
- Test-only code belongs in `src/tests/support/`, never in `src/internal/`.
- New behaviour needs a Go test; API-visible behaviour also needs a `.bru` request.
  The fake CLI (`src/tests/support/fakeclaude/auto.go`) is driven by its input
  and `[fake:...]` directives; extend it there rather than adding test hooks to the server.
- Configuration comes only from environment variables (`src/internal/config`);
  add new variables to `OwnEnvVars`, `.env.example` and `docs/ENVIRONMENT.md`.
- The `claude` process runs with built-in tools disabled; do not enable tools
  or settings sources, which would let API clients act inside the container.
- Format with gofmt. Keep comments and docs plain; no em-dashes.

## Gotchas

- `claude setup-token` is a TUI: it is driven in a 1000-column PTY and the URL
  is read from its OSC 8 hyperlink (`src/internal/claude/ansi.go`).
- Tool calls are detected from raw stream events; the process is killed at
  `message_stop` with `stop_reason: tool_use`. The MCP bridge never answers `tools/call`.
- `CLAUDE_BIN` is made absolute in config because claude runs in its own working directory.
- Integration tests re-execute the test binary as the fake CLI; every test
  package that starts claude calls `fakeclaude.MaybeRun()` in `TestMain`.
