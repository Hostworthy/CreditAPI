# API

Two surfaces: the OpenAI-compatible API used by apps, and the admin API used by
the `/admin` and `/login` pages (and scripts). Usage and setup are in
[README.md](../README.md); environment variables in
[CONFIGURATION.md](CONFIGURATION.md).

## Table of contents

- [OpenAI-compatible API](#openai-compatible-api)
  - [Authentication](#authentication)
  - [Endpoints](#endpoints)
  - [Compatibility](#compatibility)
  - [Models](#models)
  - [Tool calling](#tool-calling)
  - [Streaming](#streaming)
  - [Errors](#errors)
- [Admin API](#admin-api)
- [Health](#health)

## OpenAI-compatible API

### Authentication

Send a virtual key created at `/admin`:

```http
Authorization: Bearer sk-cmta-...
```

`X-Api-Key: sk-cmta-...` and `Api-Key: sk-cmta-...` work too. Missing,
unknown or disabled keys get `401` with code `invalid_api_key`.

### Endpoints

Every path works with and without the `/v1` prefix.

| Method | Path                       | Description                          |
| ------ | -------------------------- | ------------------------------------ |
| `POST` | `/v1/chat/completions`     | Chat completion (streaming or not)   |
| `GET`  | `/v1/models`               | Configured models and aliases        |
| `GET`  | `/v1/models/{id}`          | One model (`404` in strict mode)     |

```bash
curl https://your-server/v1/chat/completions \
  -H "Authorization: Bearer sk-cmta-..." -H "Content-Type: application/json" \
  -d '{"model":"sonnet","messages":[{"role":"user","content":"Hi!"}]}'
```

### Compatibility

| OpenAI feature                                            | Support                                                              |
| --------------------------------------------------------- | -------------------------------------------------------------------- |
| `messages` (system, developer, user, assistant, tool)     | Yes. Multi-turn history is sent to Claude as a transcript.           |
| `stream`, `stream_options.include_usage`                  | Yes                                                                  |
| `tools`, `tool_choice` (auto, none, required, named)      | Yes, plus `parallel_tool_calls` and legacy `functions`               |
| `image_url` content parts                                 | Data URL, bare base64 or http(s) link; JPEG, PNG, GIF, WebP          |
| `response_format` `json_schema`                           | Schema-validated structured output                                   |
| `response_format` `json_object`                           | JSON only, code fences stripped                                      |
| `max_tokens` / `max_completion_tokens`                    | Yes, minimum 256 so tool calls are not cut off                       |
| `stop`                                                    | Yes (string or list)                                                 |
| `reasoning_effort`                                        | Mapped to Claude Code `--effort` (`minimal` becomes `low`)            |
| `temperature`, `top_p`, `seed`, ...                       | Accepted and ignored                                                 |
| `n > 1`, logprobs, audio, embeddings, `/v1/audio/*`       | Not supported (`n > 1` returns `400 unsupported_n`)                  |

Images: the longest side is scaled to `IMAGE_MAX_DIMENSION` (1568 px) and EXIF
rotation is applied, so phone photos arrive upright and small. HEIC/AVIF are
rejected with a hint to send JPEG. Only the newest `MAX_IMAGES` images of a
conversation are sent; older ones become `[earlier image omitted]`.

### Models

`opus`, `sonnet` and `haiku` always mean the newest model of each family; full
`claude-...` names are passed through. Names from `MODEL_ALIASES` are mapped
(for example `gpt-4o=sonnet`). Any other name falls back to `DEFAULT_MODEL`,
unless `STRICT_MODELS=true`, which returns `404 model_not_found`. Responses echo
the model name the client sent.

### Tool calling

The client's functions are given to Claude as real tools with their JSON
schemas. When Claude decides to call one, the response ends with
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
{"error": {"message": "...", "type": "authentication_error", "param": null, "code": "claude_login_required"}}
```

| Status | `type`                  | `code`                    | When                                                         |
| ------ | ----------------------- | ------------------------- | ------------------------------------------------------------ |
| 400    | `invalid_request_error` | `invalid_json`, `invalid_messages`, `invalid_tools`, `invalid_response_format`, `unsupported_n`, `invalid_request` | Bad request or rejected by Claude |
| 401    | `invalid_request_error` | `invalid_api_key`         | Missing, unknown or disabled virtual key                     |
| 401    | `authentication_error`  | `claude_login_required`   | Claude is signed out or the token expired; message links to `/login` |
| 404    | `invalid_request_error` | `model_not_found`         | Unknown model with `STRICT_MODELS=true`                      |
| 413    | `invalid_request_error` | `request_too_large`       | Body over `MAX_BODY_MB`                                      |
| 429    | `rate_limit_error`      | `rate_limit_exceeded`     | Subscription usage limit reached                             |
| 429    | `rate_limit_error`      | `server_busy`             | All `MAX_CONCURRENT` slots busy for `QUEUE_TIMEOUT`          |
| 502    | `api_error`             | `upstream_error`          | The claude CLI failed                                        |
| 503    | `service_unavailable`   | `overloaded`              | Claude is overloaded                                         |
| 504    | `api_error`             | `timeout`                 | Over `REQUEST_TIMEOUT`                                       |

`claude_login_required` is a 401 on purpose: OpenAI client libraries do not
retry it. After one such result, new requests fail immediately for up to 5
minutes without starting `claude`, until the saved token changes or `/login`
re-checks.

## Admin API

Authenticate with the admin session cookie (set by `POST /admin/api/session`)
or with `Authorization: Bearer <ADMIN_PASSWORD>`. Cookie requests that change
state must come from the same origin. After 10 wrong passwords in 10 minutes an
address is locked out (`429`).

| Method   | Path                              | Body                         | Description                                                  |
| -------- | --------------------------------- | ---------------------------- | ------------------------------------------------------------ |
| `POST`   | `/admin/api/session`              | `{"password"}`               | Sign in; sets the `cmta_admin` cookie (7 days)               |
| `DELETE` | `/admin/api/session`              |                              | Sign out                                                     |
| `GET`    | `/admin/api/me`                   |                              | `{"authenticated": bool}`                                    |
| `GET`    | `/admin/api/keys`                 |                              | List keys with usage counters (never secrets or hashes)      |
| `POST`   | `/admin/api/keys`                 | `{"name"}`                   | Create a key; the `secret` is returned only here             |
| `PATCH`  | `/admin/api/keys/{id}`            | `{"name"?, "disabled"?}`     | Rename, disable or enable                                    |
| `DELETE` | `/admin/api/keys/{id}`            |                              | Delete                                                       |
| `GET`    | `/admin/api/claude`               |                              | Token source and expiry, login state, sign-in flow, usage windows, CLI version |
| `POST`   | `/admin/api/claude/check`         | `{"force"?}`                 | Is the login working? See below                              |
| `POST`   | `/admin/api/claude/login`         |                              | Start a sign-in; returns the Claude sign-in `url`            |
| `POST`   | `/admin/api/claude/login/code`    | `{"code"}`                   | Submit the code Claude shows (`409` if no sign-in is waiting) |
| `DELETE` | `/admin/api/claude/login`         |                              | Cancel the sign-in                                           |
| `PUT`    | `/admin/api/claude/token`         | `{"token"}`                  | Save a token from `claude setup-token`                       |
| `DELETE` | `/admin/api/claude/token`         |                              | Remove the saved token                                       |

**Login check.** `POST /admin/api/claude/check` reuses a successful request or
check from the last 10 minutes (`"cached": true`); otherwise it sends a one-word
prompt to Claude. `state` is `valid`, `login_required` or `error` (could not
tell, for example a network problem). `{"force": true}` always re-tests.

**Sign-in states** (`login.state`): `idle`, `starting`, `waiting_for_code`,
`submitting`, `success`, `failed`. While a sign-in waits for its code, starting
the page again resumes it rather than creating a new link.

```bash
curl -X POST https://your-server/admin/api/keys \
  -H "Authorization: Bearer $ADMIN_PASSWORD" -H "Content-Type: application/json" \
  -d '{"name":"sparkyfitness"}'
```

## Health

`GET /healthz` (no authentication):

```json
{"ok": true, "claude_token": true, "claude_login": "valid"}
```

`claude_login` is `valid`, `login_required` or `unknown` (nothing has been
tried since the server started or the token changed). Every response carries
an `X-Request-Id` header; a valid incoming `X-Request-Id` is reused.
