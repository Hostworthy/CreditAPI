# CLAUDE.md

Guidance for AI assistants working on this repository. Human-facing docs:
[README.md](README.md), [docs/SETUP.md](docs/SETUP.md), [docs/DEV.md](docs/DEV.md).

## What This Is

A Go server that exposes an OpenAI-compatible API (`/v1/chat/completions`,
`/v1/models`) backed by AI subscriptions: the Claude Code CLI (`claude -p`,
stream-json) and the Cursor CLI (`cursor-agent -p`, stream-json). Several
subscriptions can be connected; each virtual API key may use a chosen set of
them. `/admin` manages subscriptions, keys and which key uses which; `/login`
is the phone-friendly sign-in per subscription. Any OpenAI-compatible app can
be a client.

## Commands

```bash
make check      # tidy, gofmt, vet, tests: run before committing
make ci         # what CI runs: gofmt diff, vet, race tests
make e2e-ci     # Bruno end-to-end suite on the fake CLIs (needs Node)
make e2e        # server on the fake CLIs for manual testing (.env.bruno)
make lint       # staticcheck and govulncheck (also a CI job, and weekly)
```

## Layout

- `src/cmd/creditapi`: entrypoint. `src/cmd/fakeclaude`, `src/cmd/fakecursor`: fake CLI binaries.
- `src/internal/agent`: provider-neutral runtime (Backend interface, Execute,
  MCP bridge, PTY login, credential store); `agent/claude` and `agent/cursor`
  are the backends.
- `src/internal/subscriptions`: subscription store, per-key grants, routing
  (`Candidates`), per-subscription accounts and first-start migration.
- `src/internal/{admin,config,keys,logging,openai,server}`: the rest of the application.
- `src/tests`: integration tests on the full server; `src/tests/support`: helpers
  and the fake CLIs (`fakecore`, `fakeclaude`, `fakecursor`, `fakes`).
- `bruno/`: Bruno collection (`.bru`), ordered folders, `environments/local.bru`.
  Edit the `.bru` files by hand; there is no generator.
- `internal/forwarded`: which client address, scheme and host a request came
  from. Forwarding headers count only from `TRUSTED_PROXIES`; never read
  `X-Forwarded-*` directly.
- `docs/`: SETUP, DEV, API, ENVIRONMENT, SECURITY.

## Conventions

- Logging: `slog` with event-ID messages built by `logging.Event(op, action, status)`,
  e.g. `creditapi.chat.completion.succeeded`; details in attributes; use
  `logging.From(ctx)` inside handlers for the request ID. Never log tokens,
  keys, credentials, passwords, sign-in codes, prompts or responses.
- Errors to API clients use the OpenAI envelope (`type`, `code`, `message`);
  map new failure kinds in `openai.describeError` and document them in `docs/API.md`.
  Backends report failures as `*agent.Error` (use `agent.Classify` for CLI
  messages); `Retryable()` kinds make the handler try the next subscription.
- A new provider implements `agent.Backend`, is registered in `server.New` and
  gets a fake under `src/tests/support/` plus tests against it.
- Test-only code belongs in `src/tests/support/`, never in `src/internal/`.
- New behaviour needs a Go test; API-visible behaviour also needs a `.bru` request.
  The fakes are driven by their input and `[fake:...]` directives
  (`src/tests/support/fakecore/core.go`); extend them rather than adding test
  hooks to the server.
- Configuration comes only from environment variables (`src/internal/config`);
  add new variables to `OwnEnvVars`, `.env.example` and `docs/ENVIRONMENT.md`.
  Subscriptions and grants are data managed through the admin API, not config.
- The `claude` process runs with built-in tools disabled; do not enable tools
  or settings sources, which would let API clients act inside the container.
  `cursor-agent` cannot disable its tools: keep the per-run workspace, the
  deny rules in `cursor.Backend.permissions` and the absence of `--force`.
- Each subscription's CLI gets only its own credential and `HOME`
  (`agent.EnvBuilder` drops shared config locations); keep it that way.
- Secrets: register any new credential with `logging.RegisterSecret`, and pass
  text that reaches clients through the handler's `clean`.
- Client text placed inside prompt structure goes through `agent.EscapeTags`.
- Format with gofmt. Keep comments and docs plain; no em-dashes.

## Gotchas

- `claude setup-token` is a TUI: it is driven in a 1000-column PTY and the URL
  is read from its OSC 8 hyperlink (`src/internal/agent/ansi.go`).
  `cursor-agent login` prints a plain URL and exits by itself once approved.
- Claude tool calls are detected from raw stream events; the process is killed at
  `message_stop` with `stop_reason: tool_use`. Cursor tool calls are detected when
  they reach the MCP bridge (`Registration.Calls`). The bridge never answers `tools/call`.
- Cursor's partial output repeats the full text in a buffered `assistant` event
  with `model_call_id`; only deltas (`timestamp_ms`, no `model_call_id`) are streamed.
- `CLAUDE_BIN` and `CURSOR_BIN` are made absolute in config because the CLIs run
  in their own working directories.
- Integration tests re-execute the test binary as the fake CLIs; every test
  package that starts a CLI calls `fakes.MaybeRun()` in `TestMain`, and the
  binary's name (`claude` or `cursor-agent` symlink) picks the fake.
- The Cursor integration was written from Cursor's CLI documentation and the
  fake follows it; the real CLI was not run while building it.
