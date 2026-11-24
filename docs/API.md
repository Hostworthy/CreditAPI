# API

Two surfaces: the OpenAI-compatible API used by apps, and the admin API used by
the `/admin` and `/login` pages (and scripts). Setup is in
[SETUP.md](SETUP.md); environment variables in
[ENVIRONMENT.md](ENVIRONMENT.md).

## Table of contents

- [OpenAI-compatible API](#openai-compatible-api)
  - [Authentication](#authentication)
  - [Endpoints](#endpoints)
  - [Compatibility](#compatibility)
  - [Models](#models)
  - [Tool calling](#tool-calling)
  - [Streaming](#streaming)
  - [Errors](#errors)
- [Anthropic Messages API](#anthropic-messages-api)
- [Admin API](#admin-api)
- [Health](#health)

## OpenAI-compatible API

### Authentication

Send a virtual key created at `/admin`:

```http
Authorization: Bearer sk-credit-...
```

`X-Api-Key: sk-credit-...` and `Api-Key: sk-credit-...` work too. Keys created
before the rename (`sk-cmta-...`) keep working. Missing,
unknown or disabled keys get `401` with code `invalid_api_key`.

### Endpoints

Every path works with and without the `/v1` prefix.

| Method | Path                       | Description                          |
| ------ | -------------------------- | ------------------------------------ |
| `POST` | `/v1/chat/completions`     | Chat completion (streaming or not)   |
| `GET`  | `/v1/models`               | Models the key can use, including pinned `slug/model` names |
| `GET`  | `/v1/models/{id}`          | One model (`404` when no subscription of the key can serve it) |

```bash
curl https://your-server/v1/chat/completions \
  -H "Authorization: Bearer sk-credit-..." -H "Content-Type: application/json" \
  -d '{"model":"sonnet","messages":[{"role":"user","content":"Hi!"}]}'
```

### Compatibility

| OpenAI feature                                            | Support                                                              |
| --------------------------------------------------------- | -------------------------------------------------------------------- |
| `messages` (system, developer, user, assistant, tool)     | Yes. Multi-turn history is sent to the CLI as a transcript.          |
| `stream`, `stream_options.include_usage`                  | Yes                                                                  |
| `tools`, `tool_choice` (auto, none, required, named)      | Yes, plus `parallel_tool_calls` and legacy `functions`               |
| `image_url` content parts                                 | Data URL, bare base64 or http(s) link; JPEG, PNG, GIF, WebP          |
| `response_format` `json_schema`                           | Schema-validated structured output                                   |
| `response_format` `json_object`                           | JSON only, code fences stripped; the plain reply if it has no JSON |
| `max_tokens` / `max_completion_tokens`                    | Yes, minimum 256 so tool calls are not cut off                       |
| `stop`                                                    | Yes (string or list)                                                 |
| `reasoning_effort`                                        | Claude: mapped to `--effort` (`minimal` becomes `low`). Cursor: ignored |
| `temperature`, `top_p`, `seed`, ...                       | Accepted and ignored                                                 |
| `n > 1`, logprobs, audio, embeddings, `/v1/audio/*`       | Not supported (`n > 1` returns `400 unsupported_n`)                  |

Images: the longest side is scaled to `IMAGE_MAX_DIMENSION` (1568 px) and EXIF
rotation is applied, so phone photos arrive upright and small. HEIC/AVIF are
rejected with a hint to send JPEG. Images with more than `IMAGE_MAX_PIXELS`
pixels are refused with `400 invalid_messages`. Only the newest `MAX_IMAGES` images of a
conversation are sent; older ones become `[earlier image omitted]`.

### Models and Subscriptions

Every request is answered by one of the subscriptions its key may use (see
[SETUP.md](SETUP.md#which-subscription-answers)):

- **Claude** accepts `opus`, `sonnet`, `haiku` (the newest model of each
  family), full `claude-...` names and the names in `MODELS`.
- **Cursor** accepts the names in `CURSOR_MODELS` (default `auto`).
- Names from `MODEL_ALIASES` are mapped first (for example `gpt-4o=sonnet`).
- Subscriptions that accept the model are tried first, in priority order; the
  key's other subscriptions follow with their default model (`DEFAULT_MODEL`,
  `CURSOR_DEFAULT_MODEL`). With `STRICT_MODELS=true` a name no subscription of
  the key accepts returns `404 model_not_found` instead.
- `<slug>/<model>` (for example `claude-work/sonnet` or `cursor-personal/auto`)
  pins one subscription by its short name. An empty model after the slash uses
  that subscription's default; a model that subscription does not offer
  returns `404 model_not_found`. A pinned request never falls back; pinning a
  subscription the key may not use, or a disabled one, returns `403`.
- When a subscription is signed out, at its usage limit, overloaded or fails
  before anything was streamed, the next candidate answers.
  A subscription that hit its usage limit rests for 60 seconds (15 after an
  overload): it is tried after the other candidates until then.

Responses echo the model name the client sent.

**Cursor differences.** The Cursor CLI has no system prompt option, so system
messages are sent as an `<instructions>` block at the top of the prompt. Images
are written to a private temporary folder for the CLI to open. Token counts in
`usage` are estimates (about 4 characters per token) because the CLI does not
report them. Structured output is requested in the prompt and the first JSON
value of the reply is returned (`502 upstream_error` if there is none).

### Tool calling

The client's functions are given to the CLI as real tools (through a private
MCP endpoint on the loopback interface) with their JSON schemas. When the model
decides to call one, the response ends with
`finish_reason: "tool_calls"` and the calls in `message.tool_calls`; nothing is
executed on the server. The client runs the tools and sends the results back as
`role: "tool"` messages, as with OpenAI. Tool names outside
`[a-zA-Z0-9_-]` are mapped and restored transparently.

### Streaming

Server-sent events in the OpenAI chunk format:

```text
data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}
data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}
data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
data: [DONE]
```

Tool calls stream as `delta.tool_calls` (`id` and `name` first, then argument
fragments). With `stream_options.include_usage` a final chunk carries `usage`.
A `: keep-alive` comment is sent every 15 seconds. An error after the stream
started is sent as `data: {"error": {...}}` followed by `data: [DONE]`.

### Errors

Errors use the OpenAI envelope:

```json
{"error": {"message": "...", "type": "authentication_error", "param": null, "code": "subscription_login_required"}}
```

| Status | `type`                  | `code`                    | When                                                         |
| ------ | ----------------------- | ------------------------- | ------------------------------------------------------------ |
| 400    | `invalid_request_error` | `invalid_json`, `invalid_messages`, `invalid_tools`, `invalid_response_format`, `unsupported_n`, `invalid_request` | Bad request or rejected by the model |
| 400    | `invalid_request_error` | `invalid_model`           | Model name with characters other than letters, digits and `. _ : / [ ] -`, or longer than 128 |
| 401    | `invalid_request_error` | `invalid_api_key`         | Missing, unknown or disabled virtual key                     |
| 401    | `authentication_error`  | `subscription_login_required` | No subscription could answer because they are signed out; the message names them and links to `/login?subscription=<id>` |
| 403    | `permission_error`      | `no_subscription`         | The key may not use any enabled subscription, or not the pinned one |
| 404    | `invalid_request_error` | `model_not_found`         | No subscription of the key accepts the model and `STRICT_MODELS=true` |
| 413    | `invalid_request_error` | `request_too_large`       | Body over `MAX_BODY_MB`                                      |
| 429    | `rate_limit_error`      | `rate_limit_exceeded`     | Usage limit reached on every subscription tried              |
| 429    | `rate_limit_error`      | `server_busy`             | All `MAX_CONCURRENT` slots busy for `QUEUE_TIMEOUT`, or `MAX_PENDING` requests already admitted |
| 502    | `api_error`             | `upstream_error`          | The CLI failed                                               |
| 503    | `service_unavailable`   | `overloaded`              | The model is overloaded                                      |
| 504    | `api_error`             | `timeout`                 | Over `REQUEST_TIMEOUT`                                       |

`subscription_login_required` is a 401 on purpose: OpenAI client libraries do
not retry it. After a subscription reports a login failure, it is skipped for
up to 5 minutes without starting its CLI, until its credential changes or
`/login` re-checks it. When several subscriptions failed, errors other than a
login failure report the first one.

## Anthropic Messages API

Apps that speak Anthropic's API can use CreditAPI too: set the base URL to
`https://your-server` (or `https://your-server/anthropic` if the app needs a
URL of its own) and use a virtual key as the API key. Keys, subscriptions,
routing and fallback are the same as for the OpenAI API, so a Cursor
subscription can answer an Anthropic request.

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/v1/messages`, `/anthropic/v1/messages` | Create a message (streaming or not) |
| `POST` | `/v1/messages/count_tokens`, `/anthropic/v1/messages/count_tokens` | Estimated input tokens (about four characters per token) |
| `GET` | `/anthropic/v1/models`, `/anthropic/v1/models/{id}` | Models the key can use, Anthropic shape |
| `GET` | `/v1/models`, `/v1/models/{id}` | Anthropic shape when the request has an `anthropic-version` header, OpenAI shape otherwise |

Send the key as `x-api-key` (what Anthropic SDKs do) or
`Authorization: Bearer`. The `anthropic-version` header is accepted and not
checked.

```bash
curl https://your-server/v1/messages \
  -H "x-api-key: sk-credit-..." -H "anthropic-version: 2023-06-01" -H "content-type: application/json" \
  -d '{"model":"claude-haiku-4-5","max_tokens":512,"messages":[{"role":"user","content":"Hi!"}]}'
```

| Anthropic feature | Support |
| --- | --- |
| `system` (string or text blocks), `messages` with text, `image` (base64 or url), `tool_use`, `tool_result` | Yes. Images in tool results become `[image omitted]`; `thinking` blocks are dropped |
| `stream` | Yes: `message_start`, `content_block_*` (text and `tool_use` with `input_json_delta`), `message_delta`, `message_stop`, `ping`, `error` |
| `tools` (custom), `tool_choice` (`auto`, `any`, `tool`, `none`, `disable_parallel_tool_use`) | Yes. Server tools (web search, code execution and others) return 400 |
| `stop_sequences` | Yes; `stop_reason` is `stop_sequence` with the matched `stop_sequence` |
| `output_format` with `json_schema` | Structured output, returned as a text block holding the JSON |
| `max_tokens` | Yes (minimum 256, as for OpenAI) |
| `temperature`, `top_p`, `top_k`, `metadata`, `thinking` | Accepted and ignored |
| Batches, Files, citations, documents | Not supported |

`stop_reason` is `end_turn`, `max_tokens`, `stop_sequence` or `tool_use`. Tool
call IDs start with `toolu_`. `usage` carries `input_tokens`, `output_tokens`
and the cache counters (estimates for Cursor).

Errors use Anthropic's envelope, `{"type": "error", "error": {"type": ..., "message": ...}}`,
with the same statuses as the OpenAI API except that overload is `529
overloaded_error`. The types are `invalid_request_error` (400),
`authentication_error` (401, including the sign-in link when a subscription is
signed out), `permission_error` (403), `not_found_error` (404),
`request_too_large` (413), `rate_limit_error` (429), `overloaded_error` (529)
and `api_error` (500, 502, 504).

## Admin API

Authenticate with the admin session cookie (set by `POST /admin/api/session`)
or with `Authorization: Bearer <ADMIN_PASSWORD>`. Cookie requests that change
state must come from the same origin. After 10 wrong passwords in 10 minutes an
address is locked out (`429`).

| Method   | Path                              | Body                         | Description                                                  |
| -------- | --------------------------------- | ---------------------------- | ------------------------------------------------------------ |
| `POST`   | `/admin/api/session`              | `{"password"}`               | Sign in; sets the `creditapi_admin` cookie (7 days)               |
| `DELETE` | `/admin/api/session`              |                              | Sign out. This signs out every browser (sessions are not stored, so the signing secret is replaced) |
| `GET`    | `/admin/api/me`                   |                              | `{"authenticated": bool}`                                    |
| `GET`    | `/admin/api/keys`                 |                              | List keys with usage counters and `subscriptions` (never secrets or hashes) |
| `POST`   | `/admin/api/keys`                 | `{"name", "subscriptions"?}` | Create a key; the `secret` is returned only here. Without `subscriptions` the key may use every subscription |
| `PATCH`  | `/admin/api/keys/{id}`            | `{"name"?, "disabled"?, "subscriptions"?}` | Rename, disable or enable, or set the subscriptions it may use |
| `DELETE` | `/admin/api/keys/{id}`            |                              | Delete (also removes it from every subscription)             |
| `GET`    | `/admin/api/subscriptions`        |                              | Providers, subscriptions in priority order, requests in flight |
| `POST`   | `/admin/api/subscriptions`        | `{"name", "provider", "keys"?}` | Add a `claude` or `cursor` subscription at the lowest priority |
| `GET`    | `/admin/api/subscriptions/{id}`   |                              | One subscription: credential, login state, sign-in flow, usage, CLI version |
| `PATCH`  | `/admin/api/subscriptions/{id}`   | `{"name"?, "disabled"?, "keys"?}` | Rename, disable or enable, or set the keys that may use it |
| `DELETE` | `/admin/api/subscriptions/{id}`   |                              | Delete it with its saved credential and CLI files            |
| `POST`   | `/admin/api/subscriptions/{id}/move` | `{"direction": "up" \| "down"}` | Change the priority; returns the full list               |
| `POST`   | `/admin/api/subscriptions/{id}/check` | `{"force"?}`                | Is the login working? See below                              |
| `POST`   | `/admin/api/subscriptions/{id}/login` |                             | Start a sign-in; returns the sign-in `url`                   |
| `POST`   | `/admin/api/subscriptions/{id}/login/code` | `{"code"}`             | Claude only: submit the code Claude shows (`409` if no sign-in is waiting) |
| `DELETE` | `/admin/api/subscriptions/{id}/login` |                             | Cancel the sign-in                                           |
| `PUT`    | `/admin/api/subscriptions/{id}/credential` | `{"credential"}`       | Save a Claude token from `claude setup-token` or a Cursor API key |
| `DELETE` | `/admin/api/subscriptions/{id}/credential` |                        | Sign out: remove the saved credential (and run `cursor-agent logout`) |

A subscription object has `id`, `name`, `slug` (the short name for pinning),
`provider`, `provider_label`, `keys`, `disabled`, `priority` (0 is first),
`credential` (`source` is `login`, `env` or empty; `hint`, `name`,
`saved_at`, and for Claude logins `expires_about`), `auth`, `runtime`
(counters, last error, Claude usage windows), `login` and `cli_version`. Names
must be 1-60 characters and their short names unique. Only one sign-in runs at
a time; starting another subscription's sign-in cancels the running one.

**Login check.** `POST .../check` reuses a successful request or check from the
last 10 minutes (`"cached": true`); otherwise it sends a one-word prompt through
the subscription. `state` is `valid`, `login_required` or `error` (could not
tell, for example a network problem). `{"force": true}` always re-tests.

**Sign-in states** (`login.state`): `idle`, `starting`, `waiting_for_code`
(Claude), `waiting_for_browser` (Cursor: finishes by itself once approved),
`submitting`, `success`, `failed`. `login.needs_code` tells which flow it is.
While a sign-in is waiting, opening the page again resumes it rather than
creating a new link.

```bash
curl -X POST https://your-server/admin/api/keys \
  -H "Authorization: Bearer $ADMIN_PASSWORD" -H "Content-Type: application/json" \
  -d '{"name":"my-app"}'
```

## Health

`GET /healthz` needs no authentication:

```json
{"ok": true, "logins_ok": true, "subscription_count": 2, "logins_needed": 0}
```

`logins_ok` is false when any enabled subscription is known to need a sign-in;
`logins_needed` counts them. With the admin session or
`Authorization: Bearer <ADMIN_PASSWORD>`, the response also lists each enabled
subscription:

```json
"subscriptions": [{"id": "1a2b3c4d5e6f", "name": "Claude (Personal)", "provider": "claude", "login": "valid"}]
```

Each `login` is `valid`, `login_required` or `unknown` (nothing has been tried
since the server started or the credential changed). Every response carries
an `X-Request-Id` header; a valid incoming `X-Request-Id` is reused.
