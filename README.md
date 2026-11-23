# claude-max-to-api

An OpenAI-compatible API server that runs on your Claude subscription. It was
built to power the AI features of [SparkyFitness](https://github.com/CodeWithCJ/SparkyFitness)
(chat with tools, food photos, nutrition labels), but works with any app that
can talk to an "OpenAI compatible" endpoint.

Behind the scenes every request is answered by the official Claude Code CLI
(`claude -p`), signed in with your Claude Pro/Max account. You sign in once
from your phone's browser at `/login`, create a virtual API key at `/admin`,
and point your app at `https://your-server/v1`.

```
SparkyFitness ──OpenAI API──▶ claude-max-to-api ──stream-json──▶ claude -p ──▶ Claude
  (virtual key)                (Go, ~10 MB RAM)                   (your subscription)
```

## Features

- **`/v1/chat/completions`** with streaming (SSE) and non-streaming responses, plus `/v1/models`.
- **Tool / function calling.** Your app's tools are handed to Claude as real
  tools, so Claude sees the exact JSON schemas. When Claude calls one, the call
  is returned to your app as `tool_calls`; your app runs it and sends the
  result back as usual. Parallel calls and streamed argument deltas are
  supported. Nothing needs configuring for this.
- **Photos.** `image_url` parts as data URLs, bare base64 or http(s) links.
  Large phone photos are downscaled (1568 px by default) and EXIF rotation is
  applied, which keeps requests fast and cheap. JPEG, PNG, GIF and WebP.
- **Structured output.** `response_format` `json_schema` uses Claude Code's
  schema-validated structured output; `json_object` returns clean JSON (code
  fences stripped). This is what SparkyFitness's food photo and label scans need.
- **Virtual API keys** created and revoked at `/admin`, stored only as SHA-256 hashes,
  with per-key request and token counters.
- **Phone-friendly Claude login** at `/login`. It first checks whether the
  current login still works and simply tells you when it does. Only when a
  sign-in is needed does it start one: the server runs `claude setup-token`,
  shows you the sign-in link, and you paste back the code Claude gives you.
- **Clear "login required" errors.** When the login has expired, API clients
  get a 401 whose message contains the link to `/login`.
- **Subscription usage** (5-hour and 7-day windows) shown on the dashboard.
- Small footprint: a static Go binary plus the native Claude Code binary in a
  Debian slim image. Idle memory is a few MB; no Node.js.

## Quick start (Docker)

```bash
git clone https://github.com/atb00ker/claude-max-to-api && cd claude-max-to-api
cp .env.example .env          # set ADMIN_PASSWORD
docker compose up -d --build
```

Then, from any browser (your phone is fine):

1. Open `http://your-server:8080/login` and enter the admin password. The page
   checks the Claude login. On a fresh install it finds none and starts a
   sign-in by itself (if the login already works it just says you're all set).
2. Tap **Open Claude sign-in** (a new tab), log in with your Claude account and approve.
3. Claude shows an authentication code. Copy it, come back to the `/login` tab
   and tap **Paste** (or paste it into the box) and **Finish sign-in**. The page
   verifies the login with a one-word test prompt. The long-lived token (valid
   for about a year) is saved in the `/data` volume.
4. Open `/admin`, create a key (for example `sparkyfitness`) and copy it. It is
   shown only once.

Reloading `/login` in the middle of a sign-in resumes it instead of starting
over, so a code you already copied stays valid. The **Paste** button needs the
page to be served over HTTPS.

> **Why a code and not a redirect?** Claude's sign-in for the CLI ends on
> Anthropic's own page, which displays a code. The server cannot receive that
> redirect, so you copy the code back. It takes a few seconds.

Put the server behind HTTPS (Caddy, Traefik, nginx, Cloudflare Tunnel...) before
exposing it to the internet. The admin session cookie is marked `Secure`
automatically when the request arrives over HTTPS (`X-Forwarded-Proto` is
respected).

### Alternative: bring your own token

Run `claude setup-token` on any computer with a browser and either paste the
`sk-ant-oat01-...` token on the `/login` page or set `CLAUDE_CODE_OAUTH_TOKEN`
in `.env`. A login done through `/login` always takes precedence over the
environment variable, so an expired token can be replaced from your phone.

## Connect SparkyFitness

In SparkyFitness open the AI settings and add a service:

| Field            | Value                                                           |
|------------------|-----------------------------------------------------------------|
| Service type     | OpenAI Compatible                                               |
| Base URL         | `http://claude-max-to-api:8080/v1` (must end in `/v1`)          |
| API key          | a virtual key from `/admin`                                     |
| Use custom model | on, model `sonnet` (or `opus`, `haiku`)                         |

Select the same service for **Vision** so photo and label scanning work. If
both apps run in Docker, attach this container to SparkyFitness's network
(see the commented `networks` block in `docker-compose.yml`). SparkyFitness
only allows private-network AI URLs for admins or when
`ALLOW_PRIVATE_NETWORK_AI=true` is set on SparkyFitness.

## When the Claude login expires

SparkyFitness treats this server like any OpenAI-compatible provider, so it
cannot open this server's login page by itself. What it receives instead is a
standard OpenAI error that explains what to do:

```
HTTP/1.1 401 Unauthorized

{"error": {"type": "authentication_error", "code": "claude_login_required",
  "message": "Claude login required: this server's Claude subscription is signed out or its token expired. Sign in at https://claude.example.com/login and then try again."}}
```

If SparkyFitness shows the provider's error message in the chat, the link is
right there; otherwise open `/login` yourself. Either way `/login` checks the
login first and starts a sign-in only when one is needed.

- Set `PUBLIC_URL` to the address you use in the browser. Without it the link
  uses the address the client called, which for SparkyFitness in Docker is an
  internal name like `http://claude-max-to-api:8080`.
- 401 is used (not 5xx) so OpenAI client libraries report it at once instead of retrying.
- After a "login required" result, further requests fail immediately for up
  to 5 minutes without starting `claude`, until the token changes or `/login`
  re-checks.
- `GET /healthz` includes `"claude_login": "valid" | "login_required" | "unknown"`,
  handy for an uptime monitor that alerts you.

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

```bash
curl https://your-server/v1/chat/completions \
  -H "Authorization: Bearer sk-cmta-..." -H "Content-Type: application/json" \
  -d '{"model":"haiku","stream":true,"messages":[{"role":"user","content":"Hi!"}]}'
```

The key can also be sent as `X-Api-Key`. Paths work with or without the
`/v1` prefix.

## API compatibility

| OpenAI feature                         | Support                                                                 |
|----------------------------------------|-------------------------------------------------------------------------|
| `messages` (system, developer, user, assistant, tool) | Yes. Multi-turn history is sent to Claude as a transcript. |
| `stream`, `stream_options.include_usage` | Yes                                                                   |
| `tools`, `tool_choice` (auto, none, required, named), `parallel_tool_calls` | Yes. Legacy `functions` too. |
| `image_url` content parts              | Yes (data URL, base64, http(s) link)                                    |
| `response_format` `json_object` / `json_schema` | Yes                                                            |
| `max_tokens` / `max_completion_tokens` | Yes (minimum 256 so tool calls are not cut off)                         |
| `stop`                                 | Yes                                                                     |
| `reasoning_effort`                     | Mapped to Claude Code `--effort` (low, medium, high, xhigh, max)        |
| `temperature`, `top_p`, `seed`, ...    | Accepted and ignored                                                    |
| `n > 1`, logprobs, audio, embeddings, `/v1/audio/*` | Not supported                                              |

Model names: `opus`, `sonnet`, `haiku` (always the latest of each) or a full
`claude-...` model name. Unknown names such as `gpt-4o` fall back to
`DEFAULT_MODEL` unless `STRICT_MODELS=true`; `MODEL_ALIASES` maps names
explicitly.

## Configuration

All settings are environment variables (see [`.env.example`](.env.example)).

| Variable | Default | Description |
|---|---|---|
| `ADMIN_PASSWORD` | (required) | Protects `/admin`, `/login` and the admin API. Minimum 8 characters. |
| `PUBLIC_URL` | | Address users open in the browser, e.g. `https://claude.example.com`. Used for the login link in errors. |
| `DEFAULT_MODEL` | `sonnet` | Model for requests without a known model name. |
| `MODELS` | `opus,sonnet,haiku` | Models listed by `/v1/models`. |
| `MODEL_ALIASES` | | e.g. `gpt-4o=sonnet,gpt-4o-mini=haiku` |
| `STRICT_MODELS` | `false` | Reject unknown model names. |
| `DEFAULT_EFFORT` | | Default `--effort` when the client sends no `reasoning_effort`. |
| `MAX_CONCURRENT` | `2` | Claude processes allowed at once. |
| `QUEUE_TIMEOUT` | `2m` | How long a request waits for a free slot before a 429. |
| `REQUEST_TIMEOUT` | `10m` | Maximum time for one request. |
| `DEFAULT_SYSTEM_PROMPT` | `You are a helpful assistant.` | Used when the client sends no system message. |
| `IMAGE_MAX_DIMENSION` | `1568` | Longest image side in pixels (0 disables resizing). |
| `MAX_IMAGES` | `20` | Only the most recent N images of a conversation are sent. |
| `ALLOW_IMAGE_URLS` | `true` | Fetch `http(s)` image links. |
| `ALLOW_PRIVATE_IMAGE_URLS` | `false` | Allow image links to private/LAN addresses. |
| `MAX_BODY_MB` | `50` | Maximum request size. |
| `CLAUDE_CODE_OAUTH_TOKEN` | | Optional token from `claude setup-token`. |
| `DATA_DIR` | `/data` in Docker, `./data` otherwise | Keys, saved login, Claude CLI state. |
| `LISTEN_ADDR` | `:8080` | Listen address. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

Other environment variables (for example `HTTPS_PROXY` or Claude Code settings)
are passed through to the `claude` CLI. Setting `ANTHROPIC_API_KEY` makes the
CLI bill that API key instead of your subscription, so leave it unset.

To pin the Claude Code version, build with
`docker compose build --build-arg CLAUDE_VERSION=2.1.323` (default `stable`,
or `latest`).

## Admin API

Everything the web pages do is also available as JSON, authenticated with
`Authorization: Bearer <ADMIN_PASSWORD>`:

```bash
curl -X POST https://your-server/admin/api/keys \
  -H "Authorization: Bearer $ADMIN_PASSWORD" -H "Content-Type: application/json" \
  -d '{"name":"sparkyfitness"}'
```

| Method and path | Purpose |
|---|---|
| `GET /admin/api/keys`, `POST /admin/api/keys`, `PATCH`/`DELETE /admin/api/keys/{id}` | Manage virtual keys |
| `GET /admin/api/claude` | Login state, token source and expiry, usage windows, CLI version |
| `POST /admin/api/claude/check` | Is the login working? Reuses a check or request from the last 10 minutes, otherwise sends a one-word prompt (`{"force": true}` always re-tests). Returns `valid`, `login_required` or `error`. |
| `POST /admin/api/claude/login`, `POST /admin/api/claude/login/code`, `DELETE /admin/api/claude/login` | Drive the sign-in flow |
| `PUT`/`DELETE /admin/api/claude/token` | Save or remove a token |
| `GET /healthz` | Health check (no auth) |

## How it works

- Each completion starts `claude -p --input-format stream-json --output-format stream-json`
  with all built-in tools disabled (`--tools ""`), no settings files, no
  session persistence, and your system prompt replacing Claude Code's own.
  API clients therefore cannot make Claude run commands or read files in the container.
- A single user message (with its images) is passed through directly. Longer
  conversations, including earlier tool calls and results, are rendered as a
  transcript in one message, so every request is stateless.
- Client tools are offered to Claude through a private, loopback-only endpoint
  (internally an MCP server, since that is how the CLI accepts extra tools)
  that exists just for that request. As soon as Claude's message ends with
  `tool_use`, the server returns the calls and stops the process; nothing is
  executed on the server.
- Usage is reported from Claude's own token counts (cache reads included in
  `prompt_tokens`).

## Resources

- Server: about 6-15 MB of RAM.
- Each active request: one `claude` process, roughly 250-350 MB of RAM while it
  runs, plus 1-2 seconds of start-up latency.
- `MAX_CONCURRENT` caps parallel processes; extra requests queue.

## Security notes

- Virtual keys are stored as SHA-256 hashes. The Claude token, keys file and
  session secret are written with mode `0600` in `/data`.
- The admin pages use a signed, `HttpOnly`, `SameSite=Strict` cookie, refuse
  cross-origin writes and lock out an address after 10 wrong passwords in 10
  minutes. Changing `ADMIN_PASSWORD` signs everyone out.
- Image links are fetched without a proxy and private addresses are refused
  unless `ALLOW_PRIVATE_IMAGE_URLS=true`.
- The container runs as an unprivileged user (uid 1000).

## Development

```bash
go test -race ./...                       # unit + integration tests (fake claude CLI)
ADMIN_PASSWORD=devpassword DATA_DIR=./data go run ./cmd/claude-max-to-api
```

Requires Go 1.24+ and, to answer real requests, the `claude` CLI on `PATH`
(or `CLAUDE_BIN`). The tests use a fake CLI that replays real stream-json
output, so they run offline.

## Limitations

- One process per request means a little start-up latency and memory per call.
- Conversation history is sent as a transcript on every request (no server-side sessions).
- Sampling parameters are ignored; `n > 1`, audio and embeddings are not supported.

## A note on terms of use

This project drives the official Claude Code CLI with your own subscription.
It is meant for personal use with your own apps. Check Anthropic's
[consumer terms](https://www.anthropic.com/legal/consumer-terms) and usage
policies for your plan, and do not share access with other people. Responses
come from an AI model and can be wrong, so double-check nutrition numbers and
anything else that matters.

## License

MIT, see [LICENSE](LICENSE).
