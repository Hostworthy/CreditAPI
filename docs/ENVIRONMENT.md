# Environment Variables

All settings are environment variables. To run the server, see
[SETUP.md](SETUP.md); for development, see [DEV.md](DEV.md).

For Docker, put them in `.env` (see [`.env.example`](../.env.example)), which
`docker-compose.yml` passes in.

When running the binary directly, `.env` in the working directory is loaded
automatically. Set `ENV_CREDITAPI=/path/to/file.env` in the shell to load
another file instead; inside an env file, `ENV_PATH=other.env` chains to another
file (up to 10 hops, relative to the current file). Variables already set in the
shell always win.

## Server

| Variable         | Default                     | Description                                                                                   |
| ---------------- | --------------------------- | --------------------------------------------------------------------------------------------- |
| `ADMIN_PASSWORD` | (required)                  | Protects `/admin`, `/login` and the admin API. At least 8 characters. Changing it signs everyone out. |
| `HOST`           | `127.0.0.1` (`0.0.0.0` in Docker) | Listen address.                                                                         |
| `PORT`           | `8080`                      | Listen port.                                                                                  |
| `PUBLIC_URL`     |                             | Address you open in the browser, e.g. `https://ai.example.com`. Used for the `/login` link in `subscription_login_required` errors; without it the link uses the address the client called. |
| `DATA_DIR`       | `./data` (`/data` in Docker) | Keys, subscriptions with their saved logins and CLI files (`subscriptions/<id>/`), and the session secret. |
| `LOG_LEVEL`      | `info`                      | `debug`, `info`, `warn` or `error`.                                                           |

Subscriptions themselves (names, provider, which keys may use them, saved
logins) are managed on `/admin`, not in the environment. Each one runs its CLI
with its own `HOME` under `$DATA_DIR/subscriptions/<id>/home`.

## Claude

| Variable                  | Default                    | Description                                                                 |
| ------------------------- | -------------------------- | --------------------------------------------------------------------------- |
| `CLAUDE_CODE_OAUTH_TOKEN` |                            | Optional token from `claude setup-token` for the `Claude` subscription created on the first start. A login done at `/login` takes precedence. |
| `CLAUDE_BIN`              | `claude`                   | Path to the Claude Code CLI (`./bin/fakeclaude` for the e2e stack).         |
| `DEFAULT_MODEL`           | `sonnet`                   | Model for Claude subscriptions when the request names no model Claude knows. |
| `MODELS`                  | `opus,sonnet,haiku`        | Claude models listed by `/v1/models` (Claude also accepts any `claude-...` name). |
| `DEFAULT_EFFORT`          |                            | `low`, `medium`, `high`, `xhigh` or `max` when the client sends no `reasoning_effort`. |

## Cursor

| Variable               | Default        | Description                                                                 |
| ---------------------- | -------------- | --------------------------------------------------------------------------- |
| `CURSOR_API_KEY`       |                | Optional Cursor API key. When set on the first start, a `Cursor` subscription is created that uses it. A key or browser sign-in done at `/login` takes precedence. |
| `CURSOR_BIN`           | `cursor-agent` | Path to the Cursor CLI (`./bin/fakecursor` for the e2e stack).             |
| `CURSOR_DEFAULT_MODEL` | `auto`         | Model for Cursor subscriptions when the request names no model Cursor knows. |
| `CURSOR_MODELS`        | `auto`         | Model names Cursor subscriptions accept and `/v1/models` lists, e.g. `auto,gpt-5,sonnet-4.5`. Use the names `cursor-agent --list-models` shows for your plan. |

## Models

| Variable                  | Default                    | Description                                                                 |
| ------------------------- | -------------------------- | --------------------------------------------------------------------------- |
| `MODEL_ALIASES`           |                            | Extra names, e.g. `gpt-4o=sonnet,gpt-4o-mini=haiku`. Mapped before choosing a subscription. |
| `STRICT_MODELS`           | `false`                    | Reject model names no subscription of the key accepts with `404` instead of using each subscription's default model. |
| `DEFAULT_SYSTEM_PROMPT`   | `You are a helpful assistant.` | Used when the client sends no system message.                          |

## Limits

| Variable          | Default | Description                                                        |
| ----------------- | ------- | ------------------------------------------------------------------ |
| `MAX_CONCURRENT`  | `2`     | CLI processes allowed at once, across all subscriptions (each uses roughly 250-350 MB). |
| `QUEUE_TIMEOUT`   | `2m`    | How long a request waits for a free slot before `429 server_busy`. Seconds or a Go duration. |
| `REQUEST_TIMEOUT` | `10m`   | Maximum time for one request.                                      |
| `MAX_BODY_MB`     | `50`    | Maximum request body size.                                         |

## Images

| Variable                   | Default | Description                                                    |
| -------------------------- | ------- | -------------------------------------------------------------- |
| `IMAGE_MAX_DIMENSION`      | `1568`  | Longest side in pixels; `0` disables resizing.                 |
| `MAX_IMAGES`               | `20`    | Only the newest N images of a conversation are sent.           |
| `ALLOW_IMAGE_URLS`         | `true`  | Download `http(s)` image links.                                |
| `ALLOW_PRIVATE_IMAGE_URLS` | `false` | Also allow links to private, loopback and link-local addresses. |

## Passed to the CLIs

Every other environment variable is passed to the `claude` and `cursor-agent`
CLIs, so `HTTPS_PROXY` and similar settings work as usual. Not passed on:

- the variables on this page, including `CLAUDE_CODE_OAUTH_TOKEN`,
  `CURSOR_API_KEY` and `ADMIN_PASSWORD` (each subscription gets only its own
  credential);
- `ANTHROPIC_API_KEY`, which would make the CLI bill that key instead of your
  subscription (a warning is logged when it is set);
- `HOME`, `CLAUDE_CONFIG_DIR`, `CURSOR_CONFIG_DIR` and the `XDG_*_HOME`
  directories, so subscriptions never share CLI files.

This server sets `DISABLE_AUTOUPDATER=1`,
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `ENABLE_TOOL_SEARCH=false`
unless you set them.

## Docker build arguments

| Argument         | Default  | Description                                                    |
| ---------------- | -------- | -------------------------------------------------------------- |
| `CLAUDE_VERSION` | `stable` | `stable`, `latest` or an exact Claude Code version (`2.1.323`). |
| `CURSOR_VERSION` | `latest` | `latest`, an exact Cursor CLI version (`2026.11.22-e9042b2`) or `none` for an image without Cursor (about 540 MB smaller). |
| `VERSION`        | `dev`    | Version reported by `creditapi version` and in logs.   |

```bash
docker compose build --build-arg CLAUDE_VERSION=2.1.323 --build-arg CURSOR_VERSION=none
```
