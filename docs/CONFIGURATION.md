# Configuration

All settings are environment variables. For Docker, put them in `.env` (see
[`.env.example`](../.env.example)), which `docker-compose.yml` passes in.

When running the binary directly, `.env` in the working directory is loaded
automatically. Set `ENV_CLAUDE_MAX_TO_API=/path/to/file.env` in the shell to load
another file instead; inside an env file, `ENV_PATH=other.env` chains to another
file (up to 10 hops, relative to the current file). Variables already set in the
shell always win.

## Server

| Variable         | Default                     | Description                                                                                   |
| ---------------- | --------------------------- | --------------------------------------------------------------------------------------------- |
| `ADMIN_PASSWORD` | (required)                  | Protects `/admin`, `/login` and the admin API. At least 8 characters. Changing it signs everyone out. |
| `HOST`           | `127.0.0.1` (`0.0.0.0` in Docker) | Listen address.                                                                         |
| `PORT`           | `8080`                      | Listen port.                                                                                  |
| `PUBLIC_URL`     |                             | Address you open in the browser, e.g. `https://claude.example.com`. Used for the `/login` link in `claude_login_required` errors; without it the link uses the address the client called. |
| `DATA_DIR`       | `./data` (`/data` in Docker) | Keys, the saved Claude login, the session secret and Claude CLI state.                       |
| `LOG_LEVEL`      | `info`                      | `debug`, `info`, `warn` or `error`.                                                           |

## Claude

| Variable                  | Default                    | Description                                                                 |
| ------------------------- | -------------------------- | --------------------------------------------------------------------------- |
| `CLAUDE_CODE_OAUTH_TOKEN` |                            | Optional token from `claude setup-token`. A login done at `/login` takes precedence. |
| `CLAUDE_BIN`              | `claude`                   | Path to the Claude Code CLI (`./bin/fakeclaude` for the e2e stack).         |
| `CLAUDE_HOME`             | `$DATA_DIR/claude-home`    | `HOME` for the CLI.                                                          |
| `DEFAULT_MODEL`           | `sonnet`                   | Model for requests without a known model name.                              |
| `MODELS`                  | `opus,sonnet,haiku`        | Models listed by `/v1/models`.                                              |
| `MODEL_ALIASES`           |                            | Extra names, e.g. `gpt-4o=sonnet,gpt-4o-mini=haiku`.                        |
| `STRICT_MODELS`           | `false`                    | Reject unknown model names with `404` instead of using `DEFAULT_MODEL`.     |
| `DEFAULT_EFFORT`          |                            | `low`, `medium`, `high`, `xhigh` or `max` when the client sends no `reasoning_effort`. |
| `DEFAULT_SYSTEM_PROMPT`   | `You are a helpful assistant.` | Used when the client sends no system message.                          |

## Limits

| Variable          | Default | Description                                                        |
| ----------------- | ------- | ------------------------------------------------------------------ |
| `MAX_CONCURRENT`  | `2`     | `claude` processes allowed at once (each uses roughly 250-350 MB). |
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

## Passed to the Claude CLI

Every other environment variable is passed to the `claude` CLI, so Claude Code
settings and `HTTPS_PROXY` work as usual. This server sets
`DISABLE_AUTOUPDATER=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and
`ENABLE_TOOL_SEARCH=false` unless you set them. Do not set `ANTHROPIC_API_KEY`:
the CLI would bill that key instead of your subscription (a warning is logged).

## Docker build arguments

| Argument         | Default  | Description                                                    |
| ---------------- | -------- | -------------------------------------------------------------- |
| `CLAUDE_VERSION` | `stable` | `stable`, `latest` or an exact Claude Code version (`2.1.323`). |
| `VERSION`        | `dev`    | Version reported by `claude-max-to-api version` and in logs.   |

```bash
docker compose build --build-arg CLAUDE_VERSION=2.1.323
```
