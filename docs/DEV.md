# Developer Documentation

Developer documentation for claude-max-to-api. For a product overview, see
[README.md](../README.md); to run it, see [SETUP.md](SETUP.md).

## Prerequisites

- Go 1.26+
- Make
- Node.js 22+ (only for the Bruno CLI, fetched with `npx`)
- [Bruno](https://www.usebruno.com/) app (optional, to run the end-to-end collection interactively)
- Docker (optional, to build the image)
- The [Claude Code](https://code.claude.com/docs) CLI (only to run against real Claude)

## Quick Start

```bash
git clone git@github.com:atb00ker/claude-max-to-api.git
cd claude-max-to-api
cp .env.example .env          # set ADMIN_PASSWORD
make install
make check                    # tidy, format, vet, unit + integration tests
```

Run the server:

```bash
make dev                      # real claude CLI from PATH, http://127.0.0.1:8080
```

No Claude account? Run the same server on the fake Claude CLI:

```bash
cp .env.bruno.example .env.bruno
make e2e                      # http://127.0.0.1:18180, admin password: bruno-admin-password
```

Sign in at `/login` with a code that starts with `fake-code` (for example
`fake-code-123`) and everything works offline.

## Make Commands

| Command          | Description                                                            |
| ---------------- | ---------------------------------------------------------------------- |
| `make help`      | List targets                                                           |
| `make install`   | Download Go module dependencies                                        |
| `make update`    | Refresh modules within `go.mod` constraints                            |
| `make upgrade`   | Upgrade modules to latest                                              |
| `make dev`       | Run the server against the real `claude` CLI (`.env`)                 |
| `make kill`      | Kill the server listening on `PORT`                                    |
| `make build`     | Build `bin/claude-max-to-api`                                          |
| `make fake`      | Build `bin/fakeclaude`, the fake Claude CLI                            |
| `make test`      | Go unit and integration tests                                          |
| `make check`     | tidy, format, vet and test                                             |
| `make ci`        | Check-only: format diff, vet and race tests                            |
| `make e2e`       | Server on the fake CLI for Bruno (`.env.bruno`, fresh `data/bruno`)   |
| `make bruno`     | Run the Bruno collection against a running `make e2e` stack            |
| `make e2e-ci`    | Start the stack, run Bruno headless, stop it (what CI runs)            |
| `make docker`    | Build the Docker image                                                 |
| `make clean`     | Remove build artifacts and e2e data                                    |

## Package Map

```text
src/
  cmd/
    claude-max-to-api/   # server entrypoint (also `version` and `healthcheck`)
    fakeclaude/          # fake Claude CLI binary for local end-to-end testing
  internal/
    admin/               # /admin and /login pages, session cookie, admin API
      web/               # embedded HTML, CSS and JS (no build step)
    claude/              # claude -p runner, stream-json parsing, MCP bridge,
                         # login over a PTY, token store, runtime status
    config/              # environment variables and .env loading
    keys/                # virtual API keys (hashed, JSON file)
    logging/             # JSON logs, event IDs, redaction
    openai/              # OpenAI request mapping, images, HTTP handlers
    server/              # wiring, request IDs, access logs, lifecycle
  tests/                 # integration tests: the full server on the fake CLI
    support/             # test helpers and the fake Claude CLI
bruno/                   # Bruno end-to-end collection
scripts/                 # install-claude.sh (Docker image), e2e.sh (headless e2e)
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

- **Alternate entry:** `export ENV_CLAUDE_MAX_TO_API=/path/to/file.env` in the shell.
- **Chaining:** `ENV_PATH=secrets.env` inside an env file loads another file
  (relative to the current one, up to 10 hops, merged additively).

`make e2e` uses `ENV_CLAUDE_MAX_TO_API=.env.bruno`.

## How It Works

### Completions

Each request to `/v1/chat/completions` becomes one `claude -p` process:

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
- The runner reads the raw stream events (`message_start`,
  `content_block_delta`, `message_delta`, ...) and turns them into OpenAI
  chunks or a single response. Usage comes from Claude's own counts.
- A semaphore (`MAX_CONCURRENT`) limits processes; each runs in its own process
  group and is killed when the client disconnects or the request times out.

### Tool Calls

The client's functions are registered on a loopback MCP endpoint (`claude.Bridge`)
that exists only for that request, and passed with `--mcp-config`. Claude sees
them as `mcp__oai__<name>` with their exact JSON schemas. When a message ends
with `stop_reason: tool_use`, the runner returns the calls as OpenAI
`tool_calls` and stops the process; the bridge never executes anything.

### Structured Output

`response_format: json_schema` uses `--json-schema`, which makes Claude answer
through a `StructuredOutput` tool; the validated object is returned as the
message content. `json_object` uses an instruction in the system prompt and
server-side JSON extraction (code fences are stripped), because an empty schema
lets Claude return `{}`.

### Login

`/login` runs `claude setup-token` inside a 1000-column pseudo-terminal, reads
the sign-in URL from its OSC 8 hyperlink, writes the pasted code back and saves
the printed token with mode `0600`. A login saved this way takes precedence
over `CLAUDE_CODE_OAUTH_TOKEN`.

Every request records whether the login works. While it is known to be broken,
new requests fail with `401 claude_login_required` without starting `claude`
for up to 5 minutes; the state resets whenever the token changes. The `/login`
page calls `POST /admin/api/claude/check` first and only starts a sign-in when
Claude says one is needed.

### Images

Image parts are decoded (data URL, bare base64 or an SSRF-guarded download),
checked by content sniffing, rotated using EXIF orientation and scaled to
`IMAGE_MAX_DIMENSION` before being sent as base64 image blocks.

## Testing

No test needs a Claude account or network access: the AI and the login are
provided by a fake Claude CLI.

### Fake Claude CLI

[`src/tests/support/fakeclaude`](../src/tests/support/fakeclaude) speaks the
same stream-json protocol as the real CLI (event shapes copied from real output)
and fakes `claude setup-token` in a terminal, including the OSC 8 link. Go tests
re-execute their own binary as the CLI (`fakeclaude.MaybeRun` in `TestMain`);
`make fake` builds it as `bin/fakeclaude`.

In **auto** mode (the default) it answers deterministically from its input:

| Input                                     | Behaviour                                                         |
| ----------------------------------------- | ----------------------------------------------------------------- |
| No token, another token, `...expired...`  | "Not logged in", 401 invalid token, 401 expired                   |
| Token starting `sk-ant-oat01-fake-`       | Accepted (the fake sign-in issues these)                          |
| Sign-in code starting `fake-code`         | Accepted; anything else is "Invalid code"                         |
| `--json-schema`                           | StructuredOutput with a value generated from the schema           |
| Client tools                              | Calls the tools named in the prompt (or the first) through the real bridge with schema-generated arguments; answers "Received N tool result(s)" once results come back |
| Images                                    | Reply starts "I see N image(s): image/jpeg 1568x784, ..."         |
| JSON-object instruction                   | A fenced JSON object, to test fence stripping                     |
| Anything else                             | "Fake reply to: <last user line>"                                 |

Prompt directives: `[fake:rate-limit]`, `[fake:overloaded]`,
`[fake:bad-request]`, `[fake:crash]`, `[fake:slow]` (one word every 150 ms),
`[fake:echo-system]`, `[fake:echo-input]` and `[fake:long]`. Scripted scenarios
(`CMTA_FAKE_CLAUDE=text|slow|tools|schema|auth|crash`) give fixed output for unit
tests that assert exact values.

### Go Tests

```bash
make test       # or make ci for the race detector and format check
```

- Unit tests sit next to the code in `src/internal/...`.
- Integration tests in [`src/tests`](../src/tests) start the complete server
  (`server.New`, real HTTP) on the fake CLI: the login journey, chat, streaming,
  images, tools, structured output, errors, keys, the admin session and request
  IDs. Helpers such as `NewServer`, `SignIn` and `Stream` are in `src/tests/support`.

### Bruno

The [`bruno/`](../bruno) collection exercises the running server over HTTP the
way SparkyFitness does, in order: setup and reset, admin session, virtual keys,
login required, the fake Claude sign-in, chat, tools, structured output, errors,
web pages and cleanup.

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
`authorization`, `code`, `prompt`, `content`, ...) and masks `sk-ant-...` and
`sk-cmta-...` strings anywhere. Never log tokens, keys, the admin password,
sign-in codes, prompts or responses.

`message` is a stable event ID, not a sentence:
`claude-max-to-api.<operation>.<action>.<status>`, with hyphens joining words
(`login.setup-token.started`). Details go in extra fields. HTTP access lines use
`claude-max-to-api.http.request.completed`; health checks and assets log at `debug`.

| Field                                     | Description                                             |
| ----------------------------------------- | ------------------------------------------------------- |
| `timestamp`                               | ISO-8601 UTC                                            |
| `level`                                   | `debug`, `info`, `warn` or `error`                      |
| `service`                                 | `claude-max-to-api`                                     |
| `message`                                 | Event ID `^[a-z0-9-]+(\.[a-z0-9-]+){2,4}$`              |
| `request_id`                              | HTTP only; echoed in the `X-Request-Id` response header |
| `method`, `path`, `status`, `duration_ms` | HTTP access logs                                        |
| `key`, `key_id`                           | Virtual key name and id (never the secret)              |
| `model`, `stream`, `images`, `tools`, `finish`, `tool_calls`, `prompt_tokens`, `completion_tokens` | Chat completions |
| `error`                                   | Sanitized failure detail                                |

## Documentation

| Document                         | Contents                                              |
| -------------------------------- | ----------------------------------------------------- |
| [README.md](../README.md)        | What the project is                                    |
| [SETUP.md](SETUP.md)             | Running it, connecting Claude and SparkyFitness       |
| [API.md](API.md)                 | OpenAI compatibility, errors, admin API, health       |
| [ENVIRONMENT.md](ENVIRONMENT.md) | Environment variables and Docker build arguments      |
| [SECURITY.md](SECURITY.md)       | Security model and recommendations                    |
| [CLAUDE.md](../CLAUDE.md)        | Guidance for AI assistants working on the code        |
