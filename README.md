<!-- markdownlint-disable MD033 MD041 -->

<div align="center">

# claude-max-to-api

[![Go 1.26+](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go&logoColor=white)](https://go.dev/)
[![MIT License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![OpenAI compatible](https://img.shields.io/badge/API-OpenAI%20compatible-412991?style=flat)](docs/API.md)
[![Docker](https://img.shields.io/badge/Docker-Supported-2496ED?style=flat&logo=docker&logoColor=white)](Dockerfile)
[![Bruno e2e](https://img.shields.io/badge/e2e-Bruno-F4AA41?style=flat)](bruno)

*Use your Claude subscription from any app that speaks the OpenAI API.*

</div>

<!-- markdownlint-enable MD033 MD041 -->

An OpenAI-compatible API server that runs on your Claude Pro/Max subscription.
It was built for the AI features of [SparkyFitness](https://github.com/CodeWithCJ/SparkyFitness)
(chat with tools, food photos, nutrition labels) and works with any app that
can talk to an "OpenAI compatible" endpoint.

Every request is answered by the official Claude Code CLI (`claude -p`). You
sign in once from your phone's browser at `/login`, create a virtual API key at
`/admin`, and point your app at `https://your-server/v1`.

```text
SparkyFitness ──OpenAI API──▶ claude-max-to-api ──stream-json──▶ claude -p ──▶ Claude
  (virtual key)                (Go, ~10 MB RAM)                   (your subscription)
```

## Table of contents

- [Features](#features)
- [Quick start](#quick-start)
- [Connect SparkyFitness](#connect-sparkyfitness)
- [When the Claude login expires](#when-the-claude-login-expires)
- [Use it from code](#use-it-from-code)
- [Resources and limits](#resources-and-limits)
- [Security highlights](#security-highlights)
- [Documentation](#documentation)
- [A note on terms of use](#a-note-on-terms-of-use)
- [License](#license)

## Features

- **OpenAI-compatible** `/v1/chat/completions` (streaming and not) and `/v1/models`
- **Tool calling**: your app's functions reach Claude as real tools with their JSON schemas; calls come back as `tool_calls` for your app to run
- **Photos**: data URLs, base64 or links; large phone photos are downscaled and rotated upright
- **Structured output**: `response_format` `json_schema` and `json_object`, as used by photo and label scans
- **Virtual API keys** per app, stored as hashes, with usage counters
- **Phone-friendly Claude login** at `/login` that checks first and only asks you to sign in when needed
- **Clear "login required" errors** that link to `/login`
- **Subscription usage** (5-hour and 7-day windows) on the dashboard
- **Small**: a static Go binary plus the native Claude Code binary; no Node.js at runtime

## Quick start

### Docker

```bash
git clone https://github.com/atb00ker/claude-max-to-api && cd claude-max-to-api
cp .env.example .env          # set ADMIN_PASSWORD (and PUBLIC_URL)
docker compose up -d --build
```

Then, from any browser (your phone is fine):

1. Open `http://your-server:8080/login` and enter the admin password. The page
   checks the Claude login; on a fresh install it starts a sign-in by itself
   (when the login already works it just says you're all set).
2. Tap **Open Claude sign-in** (a new tab), log in with your Claude account and approve.
3. Claude shows an authentication code. Come back to the `/login` tab, tap
   **Paste** (or paste it into the box) and **Finish sign-in**. The long-lived
   token (valid for about a year) is saved in the `/data` volume.
4. Open `/admin`, create a key (for example `sparkyfitness`) and copy it. It is
   shown only once.

Reloading `/login` in the middle of a sign-in resumes it, so a code you already
copied stays valid. Claude's sign-in ends on Anthropic's own page, which shows
the code; that is why you paste it instead of being redirected back.

Put the server behind HTTPS (Caddy, Traefik, nginx, Cloudflare Tunnel...) before
exposing it to the internet. Already have a token from `claude setup-token`?
Paste it on `/login` or set `CLAUDE_CODE_OAUTH_TOKEN`.

### Local development

```bash
cp .env.example .env
make dev              # needs the claude CLI on PATH
```

Without a Claude account, `make e2e` runs the server on a fake Claude CLI; see
[CONTRIBUTING.md](docs/CONTRIBUTING.md) for the tests and the Bruno collection.

## Connect SparkyFitness

In SparkyFitness open the AI settings and add a service:

| Field            | Value                                                  |
| ---------------- | ------------------------------------------------------ |
| Service type     | OpenAI Compatible                                      |
| Base URL         | `http://claude-max-to-api:8080/v1` (must end in `/v1`) |
| API key          | a virtual key from `/admin`                            |
| Use custom model | on, model `sonnet` (or `opus`, `haiku`)                |

Select the same service for **Vision** so photo and label scanning work. If
both apps run in Docker, attach this container to SparkyFitness's network (see
the commented `networks` block in `docker-compose.yml`). SparkyFitness only
allows private-network AI URLs for admins or with `ALLOW_PRIVATE_NETWORK_AI=true`.

## When the Claude login expires

SparkyFitness treats this server like any OpenAI-compatible provider, so it
cannot open this server's login page by itself. It receives a standard error
that explains what to do:

```text
HTTP/1.1 401 Unauthorized

{"error": {"type": "authentication_error", "code": "claude_login_required",
  "message": "Claude login required: this server's Claude subscription is signed out or its token expired. Sign in at https://claude.example.com/login and then try again."}}
```

If the app shows provider errors, the link is right there; otherwise open
`/login` yourself.

- Set `PUBLIC_URL` to the address you use in the browser, otherwise the link
  uses the internal address the app called.
- 401 is used so OpenAI client libraries report it at once instead of retrying.
- Further requests fail immediately (no `claude` process) until the token
  changes or `/login` re-checks, for up to 5 minutes.
- `GET /healthz` reports `claude_login` (`valid`, `login_required`, `unknown`)
  for an uptime monitor.

## Use it from code

```python
from openai import OpenAI

client = OpenAI(base_url="https://your-server/v1", api_key="sk-cmta-...")
reply = client.chat.completions.create(
    model="sonnet",
    messages=[{"role": "user", "content": "How much protein is in 200 g of Greek yogurt?"}],
)
print(reply.choices[0].message.content)
```

Model names: `opus`, `sonnet`, `haiku` (newest of each) or a full `claude-...`
name; unknown names such as `gpt-4o` use `DEFAULT_MODEL`. Full compatibility
table, errors and the admin API: [docs/API.md](docs/API.md).

## Resources and limits

- Server: about 6-15 MB of RAM.
- Each active request runs one `claude` process: roughly 250-350 MB while it
  runs and 1-2 seconds of start-up latency. `MAX_CONCURRENT` caps them; extra
  requests queue.
- Requests are stateless: history is sent as a transcript every time.
- Sampling parameters are ignored; `n > 1`, audio and embeddings are not supported.

## Security highlights

- Built-in Claude Code tools are disabled, so API clients cannot make Claude run commands or read files in the container.
- Client tools are only described to Claude; calls go back to your app and nothing runs on the server.
- Virtual keys are stored as SHA-256 hashes; the Claude token, keys and session secret are files with mode `0600`.
- Admin pages use a signed `HttpOnly`, `SameSite=Strict` cookie, refuse cross-origin writes and lock out an address after 10 wrong passwords.
- Image links to private addresses are refused unless explicitly allowed.
- Logs are JSON with secrets and prompts redacted; the container runs as uid 1000.

## Documentation

| Document                                     | Contents                                                  |
| -------------------------------------------- | --------------------------------------------------------- |
| [docs/API.md](docs/API.md)                   | OpenAI compatibility, errors, admin API, health           |
| [docs/CONFIGURATION.md](docs/CONFIGURATION.md) | Environment variables and Docker build arguments        |
| [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md) | Development, tests, fake Claude CLI, Bruno, logging, layout |

## A note on terms of use

This project drives the official Claude Code CLI with your own subscription.
It is meant for personal use with your own apps. Check Anthropic's
[consumer terms](https://www.anthropic.com/legal/consumer-terms) and usage
policies for your plan, and do not share access with other people. Responses
come from an AI model and can be wrong, so double-check nutrition numbers and
anything else that matters.

## License

MIT, see [LICENSE](LICENSE).
