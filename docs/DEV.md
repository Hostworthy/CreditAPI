# Developer Documentation

Developer documentation for CreditAPI. For a product overview, see
[README.md](../README.md); to run it, see [SETUP.md](SETUP.md).

## Prerequisites

- Go 1.26+
- Make
- Node.js 22+ (only for the Bruno CLI, fetched with `npx`)
- [Bruno](https://www.usebruno.com/) app (optional, to run the end-to-end collection interactively)
- Docker (optional, to build the image)
- The [Claude Code](https://code.claude.com/docs) CLI and/or the
  [Cursor CLI](https://cursor.com/docs/cli/overview) (only to run against real
  subscriptions)

## Quick Start

```bash
git clone git@github.com:atb00ker/CreditAPI.git
cd CreditAPI
cp .env.example .env          # set ADMIN_PASSWORD
make install
make check                    # tidy, format, vet, unit + integration tests
```

Run the server:

```bash
make dev                      # real claude and cursor-agent from PATH, http://127.0.0.1:8080
```

No account? Run the same server on the fake CLIs:

```bash
cp .env.bruno.example .env.bruno
make e2e                      # http://127.0.0.1:18180, admin password: bruno-admin-password
```

Sign a Claude subscription in at `/login` with a code that starts with
`fake-code` (for example `fake-code-123`). Add a Cursor subscription on
`/admin`; its fake browser sign-in approves itself after a moment, and a pasted
key starting with `fake-cursor-key-` works too. Everything runs offline.

## Make Commands

| Command | Description |
| --- | --- |
| `make help` | List targets |
| `make install` | Download Go module dependencies |
| `make update` | Refresh modules within `go.mod` constraints |
| `make upgrade` | Upgrade modules to latest |
| `make dev` | Run the server against the real CLIs (`.env`) |
| `make kill` | Kill the server listening on `PORT` |
| `make build` | Build `bin/creditapi` |
| `make fake` | Build `bin/fakeclaude` and `bin/fakecursor`, the fake CLIs |
| `make test` | Go unit and integration tests |
| `make check` | tidy, format, vet and test |
| `make ci` | Check-only: format diff, vet and race tests |
| `make e2e` | Server on the fake CLIs for Bruno (`.env.bruno`, fresh `data/bruno`) |
| `make bruno` | Run the Bruno collection against a running `make e2e` stack |
| `make e2e-ci` | Start the stack, run Bruno headless, stop it (what CI runs) |
| `make docker` | Build the Docker image |
| `make clean` | Remove build artifacts and e2e data |

## Package Map

```text
src/
  cmd/
    creditapi/           # server entrypoint (also `version` and `healthcheck`)
    fakeclaude/          # fake Claude CLI binary for local end-to-end testing
    fakecursor/          # fake Cursor CLI binary for local end-to-end testing
  internal/
    admin/               # /admin and /login pages, session cookie, admin API
      web/               # embedded HTML, CSS and JS (no build step)
    agent/               # what every provider shares: Backend interface,
                         # Execute (fail-fast, status), MCP bridge, login over a
                         # PTY, credential store, process and env helpers
      claude/            # Claude backend: claude -p, stream-json, setup-token
      cursor/            # Cursor backend: cursor-agent -p, hardening, login
    config/              # environment variables and .env loading
    keys/                # virtual API keys (hashed, JSON file)
    logging/             # JSON logs, event IDs, redaction
    openai/              # OpenAI request mapping, images, fallback, HTTP handlers
    subscriptions/       # subscriptions store, per-key grants, routing, accounts
    server/              # wiring, request IDs, access logs, lifecycle
  tests/                 # integration tests: the full server on the fake CLIs
    support/             # test helpers
      fakecore/          # what both fakes share (directives, schemas, MCP client)
      fakeclaude/        # fake Claude CLI
      fakecursor/        # fake Cursor CLI
      fakes/             # picks the fake from the name the binary was started as
bruno/                   # Bruno end-to-end collection
scripts/                 # install-claude.sh and install-cursor.sh (Docker image),
                         # e2e.sh (headless e2e)
docs/                    # this documentation
```

Test-only code lives under `src/tests/support/`; do not add test APIs to
`src/internal/`.

## Environment Variables

All settings are environment variables, documented in
[ENVIRONMENT.md](ENVIRONMENT.md).

### Environment Loading

The server loads `.env` from the working directory (variables already set in
the shell win). Two overrides, the same as in goauth:

- **Alternate entry:** `export ENV_CREDITAPI=/path/to/file.env` in the shell.
- **Chaining:** `ENV_PATH=secrets.env` inside an env file loads another file
  (relative to the current one, up to 10 hops, merged additively).

`make e2e` uses `ENV_CREDITAPI=.env.bruno`.

## How It Works

### Subscriptions and Routing

`subscriptions.Store` keeps the subscriptions in `DATA_DIR/subscriptions.json`
in priority order, each with its provider and the IDs of the keys that may use
it. `subscriptions.Manager` gives each one an `agent.Account`: a private `HOME`
(`DATA_DIR/subscriptions/<id>/home`), a credential store
(`DATA_DIR/subscriptions/<id>/credential`, falling back to `CLAUDE_CODE_OAUTH_TOKEN`
or `CURSOR_API_KEY` for the subscriptions created from them) and a status.

For each request, `Manager.Candidates` lists the subscriptions to try:
`slug/model` pins one; otherwise the key's enabled subscriptions whose backend
`Accepts` the model come first, then the others with their default model
(unless `STRICT_MODELS`). The OpenAI handler tries them in order and moves on
when one fails with a retryable error (login, usage limit, overloaded,
upstream) before anything was streamed. `agent.Execute` wraps every run: it
skips a subscription whose login is known to be broken and records success,
failure and login state.

On the first start (no `subscriptions.json`), `Manager.Bootstrap` creates a
`Claude` subscription that takes over an older install's `claude-oauth-token`
and `claude-home`, plus a `Cursor` one when `CURSOR_API_KEY` is set, and grants
both to every existing key.

### Claude Completions

Each run becomes one `claude -p` process:

```text
claude -p --input-format stream-json --output-format stream-json --verbose
  --include-partial-messages --model <alias> --system-prompt-file <file>
  --tools "" --setting-sources "" --strict-mcp-config --no-session-persistence
  --max-turns 1 [--mcp-config <bridge> --allowedTools mcp__oai]
  [--json-schema <schema>] [--effort <level>]
```

- Built-in tools are disabled and no settings files are read, so API clients
  cannot make Claude run commands or read files in the container.
- The client's system prompt replaces Claude Code's own.
- A single user turn is sent as content blocks (text and images). Longer
  conversations are rendered into one message as a transcript with `<user>`,
  `<assistant>`, `<tool_call>` and `<tool_result>` elements, so every request
  is stateless.
- The backend reads the raw stream events (`message_start`,
  `content_block_delta`, `message_delta`, ...) and turns them into OpenAI
  chunks or a single response. Usage comes from Claude's own counts.
- A semaphore (`MAX_CONCURRENT`, shared by all subscriptions) limits processes;
  each runs in its own process group and is killed when the client disconnects
  or the request times out.

### Cursor Completions

Each run becomes one `cursor-agent -p` process in a new temporary workspace:

```text
cursor-agent -p --output-format stream-json --stream-partial-output
  --model <name> --workspace <tmp> --trust [--approve-mcps] "<prompt>"
```

- The CLI has no system prompt option and no way to turn its tools off, so the
  prompt starts with an `<instructions>` block, images are written to
  `./attachments/` and referenced by path, and prompts over 96 KiB go to
  `./conversation.md`.
- Before every run the backend writes deny rules (shell, writes, web fetches,
  sensitive paths) to the workspace `.cursor/cli.json` and merges them, with
  `approvalMode: allowlist`, into the subscription's `cli-config.json`. See
  [SECURITY.md](SECURITY.md#cursor).
- Partial text arrives as `assistant` events with `timestamp_ms` and no
  `model_call_id`; the buffered duplicates are skipped. The `result` event ends
  the run. Token counts are estimated.
- Client tools go through the same MCP bridge (`.cursor/mcp.json` in the
  workspace). The bridge reports each `tools/call` on a channel; the backend
  collects parallel calls for a moment, stops the CLI and returns them.

### Tool Calls

The client's functions are registered on a loopback MCP endpoint
(`agent.Bridge`) that exists only for that request. The CLI sees them as
`mcp__oai__<name>` with their exact JSON schemas. Claude calls are detected in
the stream (`stop_reason: tool_use`); Cursor calls when they reach the bridge.
Either way they are returned as OpenAI `tool_calls` and the process is stopped;
the bridge never executes anything.

### Structured Output

For Claude, `response_format: json_schema` uses `--json-schema`, which makes
Claude answer through a `StructuredOutput` tool; the validated object is
returned as the message content. For Cursor the schema is part of the prompt
and the first JSON value of the reply is returned. `json_object` uses an
instruction in the system prompt and server-side JSON extraction (code fences
are stripped), because an empty schema lets Claude return `{}`.

### Login

`agent.LoginManager` runs one CLI sign-in at a time inside a 1000-column
pseudo-terminal and reads the sign-in URL from its output (OSC 8 hyperlink or
plain text):

- **Claude** (`claude setup-token`): the pasted code is written back to the
  terminal and the printed token is saved with mode `0600`. It takes precedence
  over `CLAUDE_CODE_OAUTH_TOKEN`.
- **Cursor** (`cursor-agent login` with `NO_OPEN_BROWSER=1`): the CLI finishes
  by itself once the browser sign-in is approved and keeps the session in the
  subscription's `HOME`. Signing out runs `cursor-agent logout`.

Every request records whether a subscription's login works. While it is known
to be broken, that subscription is skipped for up to 5 minutes without starting
its CLI; the state resets whenever its credential changes or a sign-in
finishes. When no subscription could answer because of logins, the client gets
`401 subscription_login_required` with a link to `/login?subscription=<id>`.
The `/login` page calls `POST /admin/api/subscriptions/{id}/check` first and
only starts a sign-in when one is needed.

### Images

Image parts are decoded (data URL, bare base64 or an SSRF-guarded download),
checked by content sniffing, rotated using EXIF orientation and scaled to
`IMAGE_MAX_DIMENSION` before being sent as base64 image blocks (Claude) or
files (Cursor).

## Testing

No test needs an account or network access: the AI and the logins are provided
by fake CLIs.

### Fake CLIs

[`fakeclaude`](../src/tests/support/fakeclaude) speaks the same stream-json
protocol as the real Claude CLI (event shapes copied from real output) and
fakes `claude setup-token` in a terminal, including the OSC 8 link.
[`fakecursor`](../src/tests/support/fakecursor) imitates `cursor-agent`
(`-p` with stream-json and partial output, `login`, `logout`, `status`,
`--version`; shapes from the Cursor CLI documentation) and fails any run whose
workspace is not hardened. What they share is in
[`fakecore`](../src/tests/support/fakecore).

Go tests re-execute their own binary as the CLIs: test packages call
`fakes.MaybeRun()` in `TestMain`, and `support.NewServer` points `CLAUDE_BIN`
and `CURSOR_BIN` at symlinks named `claude` and `cursor-agent`, so the name
picks the fake. `make fake` builds them as `bin/fakeclaude` and `bin/fakecursor`.

In **auto** mode they answer deterministically from their input:

| Input | Behaviour |
| --- | --- |
| Claude: no token, another token, `...expired...` | "Not logged in", 401 invalid token, 401 expired |
| Claude: token starting `sk-ant-oat01-fake-` | Accepted (the fake sign-in issues these) |
| Claude: sign-in code starting `fake-code` | Accepted; anything else is "Invalid code" |
| Cursor: `CURSOR_API_KEY` starting `fake-cursor-key-` | Accepted unless it contains `expired`; otherwise a stored `login` session is needed |
| Cursor: `login` | Prints a sign-in link and approves after 600 ms (`CREDITAPI_FAKE_CURSOR_LOGIN=deny` or `hang` to test failures) |
| A JSON schema | A value generated from the schema |
| Client tools | Calls the tools named in the prompt (or the first) through the real bridge with schema-generated arguments; answers "Received N tool result(s)" once results come back |
| Images | Reply starts "I see N image(s): image/jpeg 1568x784, ..." |
| JSON-object instruction | A fenced JSON object, to test fence stripping |
| Anything else | "Fake reply to: <last user line>" |

Prompt directives: `[fake:rate-limit]`, `[fake:overloaded]`,
`[fake:bad-request]`, `[fake:crash]`, `[fake:slow]` (one word every 150 ms),
`[fake:echo-system]`, `[fake:echo-input]`, `[fake:long]` and `[fake:whoami]`
("Served by cursor subscription <id>", to test routing). Scripted Claude
scenarios (`CREDITAPI_FAKE_CLAUDE=text|slow|tools|schema|auth|crash`) give fixed
output for unit tests that assert exact values.

### Go Tests

```bash
make test       # or make ci for the race detector and format check
```

- Unit tests sit next to the code in `src/internal/...`; the backends are
  tested against the fake CLIs, routing and the store with stub backends.
- Integration tests in [`src/tests`](../src/tests) start the complete server
  (`server.New`, real HTTP) on the fake CLIs: the login journeys for both
  providers, five keys sharing four subscriptions, pinning, priority, fallback,
  chat, streaming, images, tools, structured output, errors, keys, the admin
  session and request IDs. Helpers such as `NewServer`, `CreateKeyFor`,
  `CreateSubscription`, `SignInClaude`, `SignInCursor` and `Stream` are in
  `src/tests/support`.

### Bruno

The [`bruno/`](../bruno) collection exercises the running server over HTTP the
way an OpenAI-compatible app does, in order: setup and reset, admin session,
virtual keys, login required, the fake Claude sign-in, subscriptions (a Cursor
subscription with its browser sign-in, routing, pinning, priority and per-key
access), chat, tools, structured output, errors, web pages and cleanup.

```bash
make e2e        # terminal 1
make bruno      # terminal 2 (or open bruno/ in the Bruno app, environment "local")
```

The first folder resets the server, so the collection can run repeatedly
against the same stack. `make e2e-ci` runs everything headless with a temporary
data directory and writes `bruno/results/junit.xml` and `report.html`; CI
uploads them. When you change the API, update the matching `.bru` files.

## Observability and Logging

Logs are JSON lines on stdout. `LOG_LEVEL` is `debug`, `info` (default), `warn`
or `error`. A redacting handler drops sensitive attributes (`token`, `api_key`,
`authorization`, `credential`, `code`, `prompt`, `content`, ...) and masks `sk-ant-...` and
`sk-credit-...` (and legacy `sk-cmta-...`) strings anywhere. Never log tokens, keys, the admin password,
sign-in codes, prompts or responses.

`message` is a stable event ID, not a sentence:
`creditapi.<operation>.<action>.<status>`, with hyphens joining words
(`login.cli.started`). Details go in extra fields. HTTP access lines use
`creditapi.http.request.completed`; health checks and assets log at `debug`.

| Field                                     | Description                                             |
| ----------------------------------------- | ------------------------------------------------------- |
| `timestamp`                               | ISO-8601 UTC                                            |
| `level`                                   | `debug`, `info`, `warn` or `error`                      |
| `service`                                 | `creditapi`                                             |
| `message`                                 | Event ID `^[a-z0-9-]+(\.[a-z0-9-]+){2,4}$`              |
| `request_id`                              | HTTP only; echoed in the `X-Request-Id` response header |
| `method`, `path`, `status`, `duration_ms` | HTTP access logs                                        |
| `key`, `key_id`                           | Virtual key name and id (never the secret)              |
| `subscription`, `provider`, `attempts`    | Subscription id that answered (or was tried), its provider and how many were tried |
| `model`, `stream`, `images`, `tools`, `finish`, `tool_calls`, `prompt_tokens`, `completion_tokens` | Chat completions |
| `error`                                   | Sanitized failure detail                                |

## Documentation

| Document                         | Contents                                              |
| -------------------------------- | ----------------------------------------------------- |
| [README.md](../README.md)        | What the project is                                    |
| [SETUP.md](SETUP.md)             | Running it, connecting subscriptions and your apps    |
| [API.md](API.md)                 | OpenAI compatibility, errors, admin API, health       |
| [ENVIRONMENT.md](ENVIRONMENT.md) | Environment variables and Docker build arguments      |
| [SECURITY.md](SECURITY.md)       | Security model and recommendations                    |
| [CLAUDE.md](../CLAUDE.md)        | Guidance for AI assistants working on the code        |
