# Contributing

**Developers:** this guide covers local setup, testing and the contribution
workflow. Usage is in [README.md](../README.md); endpoints in [API.md](API.md);
environment variables in [CONFIGURATION.md](CONFIGURATION.md).

## Prerequisites

- Go 1.26+
- [make](https://www.gnu.org/software/make/)
- Node.js 22+ (only for the Bruno CLI; `npx` fetches it)
- [Bruno](https://www.usebruno.com/) app, optional, to run the collection interactively
- The [Claude Code](https://code.claude.com/docs) CLI, only to run against real Claude (`make dev`)

## Environment

```bash
git clone git@github.com:atb00ker/claude-max-to-api.git
cd claude-max-to-api
cp .env.example .env
```

`.env` is gitignored; never commit secrets. The server loads `.env` from the
working directory. Overrides:

- **Local chaining:** set `ENV_PATH=/path/to/secrets.env` in `.env` (up to 10 hops; merges additively).
- **Alternate entry:** `export ENV_CLAUDE_MAX_TO_API=/path/to/.env` in the shell (not inside `.env`).

## Make targets

| Target          | Description                                                                 |
| --------------- | --------------------------------------------------------------------------- |
| `make help`     | List targets                                                                |
| `make install`  | Download module dependencies                                                |
| `make dev`      | Run the server against the real `claude` CLI (`.env`)                      |
| `make kill`     | Kill the server listening on `PORT`                                         |
| `make build`    | Build `bin/claude-max-to-api`                                               |
| `make fake`     | Build `bin/fakeclaude`, the fake Claude CLI                                 |
| `make test`     | Go unit and integration tests                                              |
| `make check`    | tidy, format, vet and test                                                  |
| `make ci`       | Check-only: format diff, vet and race tests                                 |
| `make e2e`      | Run the server with the fake CLI for Bruno (`.env.bruno`), foreground       |
| `make bruno`    | Run the Bruno collection against a running `make e2e` stack                 |
| `make e2e-ci`   | Start the stack, run Bruno headless, stop it (what CI runs)                 |
| `make docker`   | Build the Docker image                                                      |
| `make update` / `make upgrade` | Refresh / upgrade Go modules                                 |
| `make clean`    | Remove build artifacts and e2e data                                         |

## Testing

Nothing in the test suite needs a Claude account or network access: the AI
side and the login are provided by a fake Claude CLI.

### The fake Claude CLI

[`src/tests/support/fakeclaude`](../src/tests/support/fakeclaude) speaks the same
`claude -p --input-format stream-json --output-format stream-json` protocol as
the real CLI (event shapes copied from real output) and fakes
`claude setup-token` in a terminal, including the OSC 8 sign-in link. Go tests
re-execute their own binary as the CLI (`fakeclaude.MaybeRun` in `TestMain`);
`make fake` builds it as `bin/fakeclaude` for the e2e stack.

In **auto** mode (the default) it answers deterministically from its input:

| Input                                   | Behaviour                                                               |
| --------------------------------------- | ----------------------------------------------------------------------- |
| No token / other token / `...expired...` | "Not logged in" / 401 invalid token / 401 expired, like the real API   |
| Token starting `sk-ant-oat01-fake-`     | Accepted (the fake sign-in issues these)                                |
| Sign-in code starting `fake-code`       | Accepted by the fake `setup-token`; anything else is "Invalid code"     |
| `--json-schema`                         | StructuredOutput with a value generated from the schema                 |
| Client tools                            | Calls the tools named in the prompt (or the first) through the real MCP bridge, arguments generated from their schemas; replies "Received N tool result(s)" once results come back |
| Images                                  | Reply starts "I see N image(s): image/jpeg 1568x784, ..."               |
| JSON-object system instruction          | Replies with a fenced JSON object (tests fence stripping)               |
| Anything else                           | "Fake reply to: <last user line>"                                       |

Directives in the prompt: `[fake:rate-limit]`, `[fake:overloaded]`,
`[fake:bad-request]`, `[fake:crash]`, `[fake:slow]` ("one two three STOP four
five", one word every 150 ms), `[fake:echo-system]`, `[fake:echo-input]`,
`[fake:long]`. Replies longer than `CLAUDE_CODE_MAX_OUTPUT_TOKENS × 4`
characters end with `max_tokens`.

Scripted scenarios (`CMTA_FAKE_CLAUDE=text|slow|tools|schema|auth|crash`) give
fixed output for unit tests that assert exact values.

### Go tests

```bash
make test       # or: make ci (adds the race detector and format check)
```

- Unit tests live next to the code in `src/internal/...`.
- Integration tests in [`src/tests`](../src/tests) start the complete server
  (`server.New`, real HTTP) on the fake CLI and walk through the login journey,
  chat, streaming, images, tools, structured output, errors, keys and the admin
  session. Helpers live in `src/tests/support` (`NewServer`, `SignIn`,
  `Stream`, ...).
- Test-only code stays under `src/tests/support/`; do not add test APIs to `src/internal/`.

### Bruno end-to-end tests

The [`bruno/`](../bruno) collection exercises the running server over HTTP the
way SparkyFitness does, in order: setup and reset, admin session, virtual keys,
login required, the fake Claude sign-in, chat, tools, structured output, errors,
web pages and cleanup.

```bash
cp .env.bruno.example .env.bruno
make e2e        # terminal 1: server + fake CLI on 127.0.0.1:18180 (fresh data/bruno)
make bruno      # terminal 2: run the collection with the Bruno CLI
```

Or open `bruno/` in the Bruno app, pick the `local` environment and run the
collection. The first folder resets the server, so it can be run repeatedly
against the same stack. `make e2e-ci` runs everything headless with a temporary
data directory and writes `bruno/results/junit.xml` and `report.html` (CI
uploads them as an artifact).

### Against real Claude

Install the Claude Code CLI, set `ADMIN_PASSWORD` in `.env`, run `make dev` and
sign in at `http://127.0.0.1:8080/login`.

## Logging

The server writes structured JSON logs to stdout. Set `LOG_LEVEL` (`debug`,
`info`, `warn`, `error`; default `info`). A redacting handler drops sensitive
attributes (`token`, `api_key`, `authorization`, `code`, `prompt`, `content`,
...) and masks `sk-ant-...` / `sk-cmta-...` strings anywhere. Never log tokens,
keys, the admin password, sign-in codes, prompts or responses.

`message` is a stable event ID, not a sentence:
`claude-max-to-api.<operation>.<action>.<status>` (four parts; hyphens join
words, e.g. `setup-token`). Put details in extra fields. HTTP access lines use
`claude-max-to-api.http.request.completed` (health checks and assets log at
`debug`).

| Field                                     | Description                                              |
| ----------------------------------------- | -------------------------------------------------------- |
| `timestamp`                               | ISO-8601 UTC                                             |
| `level`                                   | `debug`, `info`, `warn` or `error`                       |
| `service`                                 | `claude-max-to-api`                                      |
| `message`                                 | Event ID `^[a-z0-9-]+(\.[a-z0-9-]+){2,4}$`               |
| `request_id`                              | HTTP only; echoed in the `X-Request-Id` response header  |
| `method`, `path`, `status`, `duration_ms` | HTTP access logs                                         |
| `key`, `key_id`                           | Virtual key name and id (never the secret)               |
| `model`, `stream`, `images`, `tools`, `finish`, `tool_calls`, `prompt_tokens`, `completion_tokens` | Chat completions |
| `error`                                   | Sanitized failure detail                                 |

## Repository layout

```text
src/
  cmd/
    claude-max-to-api/   # server entrypoint (also `version`, `healthcheck`)
    fakeclaude/          # fake Claude CLI for e2e testing
  internal/
    admin/               # /admin and /login pages, session cookie, admin API
      web/               # embedded HTML, CSS and JS
    claude/              # claude -p runner, stream-json parsing, MCP bridge, login (PTY), token store
    config/              # environment and .env loading
    keys/                # virtual key store (hashed, JSON file)
    logging/             # JSON logs, event IDs, redaction
    openai/              # OpenAI request/response mapping, images, HTTP handlers
    server/              # wiring, request IDs, access logs, lifecycle
  tests/                 # integration tests (full server on the fake CLI)
    support/             # test helpers and the fake Claude CLI
bruno/                   # Bruno end-to-end collection
scripts/                 # install-claude.sh (Docker), e2e.sh (headless e2e)
docs/                    # API, configuration and this guide
```

## Architecture notes

- Each completion runs `claude -p` with built-in tools disabled (`--tools ""`),
  no settings files, no session persistence and the client's system prompt
  replacing Claude Code's own, so API clients cannot run commands in the container.
- A single user turn is passed through as content blocks; longer conversations
  are rendered as one transcript message, so requests are stateless.
- Client tools are served by a loopback MCP endpoint registered per request
  (`claude.Bridge`). The runner watches the raw stream events and stops the
  process at `message_stop` with `stop_reason: tool_use`, returning the calls.
- `response_format` `json_schema` uses `--json-schema` (StructuredOutput);
  `json_object` uses an instruction and server-side JSON extraction.
- `/login` drives `claude setup-token` in a 1000-column PTY and reads the URL
  from its OSC 8 hyperlink. The token is stored with mode `0600`.
- The runner records whether the login works on every request. While it is
  known to be broken, requests fail fast for up to 5 minutes; the state resets
  whenever the token changes.
