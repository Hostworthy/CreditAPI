# Security

How CreditAPI protects your subscription, your keys and your data, and
what to do when you deploy it. For setup, see [SETUP.md](SETUP.md).

## What Is Protected

| Asset                    | Where it lives                                  | Protection                                                        |
| ------------------------ | ----------------------------------------------- | ----------------------------------------------------------------- |
| Saved credentials (Claude tokens, Cursor API keys) | `DATA_DIR/subscriptions/<id>/credential` | File mode `0600`; only a redacted hint is ever shown |
| CLI sign-ins and files   | `DATA_DIR/subscriptions/<id>/home`              | One private `HOME` per subscription; never shared between subscriptions |
| Which key may use which subscription | `DATA_DIR/subscriptions.json`       | Changed only through the admin API                                |
| Virtual API keys         | `DATA_DIR/keys.json`                            | Only SHA-256 hashes are stored; the secret is shown once          |
| Admin sessions           | `creditapi_admin` cookie                             | HMAC-signed, `HttpOnly`, `SameSite=Strict`, `Secure` over HTTPS, 7 days |
| Session signing secret   | `DATA_DIR/session-secret`                       | Random per installation, mixed with `ADMIN_PASSWORD`; mode `0600` |
| Prompts, photos, replies | Memory only                                     | Not written to disk or logs; sessions are not persisted by the CLI |

## API Clients

- Every request needs a virtual key; disabled or deleted keys stop working at once.
- A key can only reach the subscriptions it was granted on `/admin`; naming
  another subscription in the model (`slug/model`) returns `403`.
- Each subscription's CLI gets only that subscription's credential and its own
  `HOME`. The server's other credentials, `ADMIN_PASSWORD` and shared config
  locations (`CLAUDE_CONFIG_DIR`, `CURSOR_CONFIG_DIR`, `XDG_*_HOME`) are never
  passed to a CLI.
- Claude Code's built-in tools (shell, files, web) are disabled and no settings
  files are loaded, so a prompt cannot make Claude run commands or read files
  in the container.
- Client tools are only described to the CLI through a loopback endpoint with a
  random path per request. Calls are returned to the client; the server never
  executes them.
- Request bodies are limited by `MAX_BODY_MB`, processes by `MAX_CONCURRENT` and
  time by `REQUEST_TIMEOUT`.

## Cursor

The Cursor CLI cannot switch its built-in tools off the way Claude Code can,
so Cursor subscriptions are hardened as far as the CLI allows:

- Every request runs in a new, empty temporary folder that is deleted
  afterwards. Images are written there for the CLI to open.
- Deny rules in the folder's `.cursor/cli.json` and in the subscription's
  `cli-config.json` (with `approvalMode: allowlist`) block shell commands, file
  writes, web fetches and reads of the data directory, the subscription's
  `HOME`, `/etc`, `/proc`, `/sys`, `/root`, `/home`, `/var` and `/run`.
- `--force` is never passed, so nothing outside the allow list is approved.
- The only MCP server is the loopback endpoint for the client's own tools.

These rules come from the Cursor CLI and depend on it enforcing them, so a
Cursor subscription gives prompts more room than a Claude one. Only give Cursor
access to keys used by apps and people you trust, and keep the container
itself locked down (non-root user, no secrets beyond what it needs). The
integration was built from Cursor's CLI documentation; check its behaviour with
your own account before relying on it.

## Admin Pages and API

- `/admin`, `/login` and `/admin/api/*` require the admin password, either as a
  signed session cookie or as `Authorization: Bearer <ADMIN_PASSWORD>`.
- Cookie requests that change state must come from the same origin.
- After 10 wrong passwords within 10 minutes an address is locked out for the
  rest of that window.
- Pages are served with a strict Content Security Policy (no inline scripts),
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` and `Cache-Control: no-store`.
- Changing `ADMIN_PASSWORD` signs every session out.

## Outbound Requests

- Image links are downloaded only when `ALLOW_IMAGE_URLS=true`, without a proxy,
  with a 20 MB limit, three redirects at most and a 30 second timeout.
- Links that resolve to private, loopback, link-local or carrier-grade NAT
  addresses are refused unless `ALLOW_PRIVATE_IMAGE_URLS=true`.
- The Claude CLI runs with auto-updates and non-essential traffic disabled.
- The Cursor CLI contacts Cursor's servers for every request and sign-in.

## Logs

Logs never contain tokens, keys, the admin password, sign-in codes, prompts or
responses. A redacting handler drops sensitive fields and masks anything that
looks like an `sk-ant-` or `sk-credit-` secret, including inside error messages.
Cursor sign-in output shown on `/login` comes from the CLI; it contains the
sign-in link but no credentials.

## Container

- Runs as the unprivileged user `app` (uid 1000) with `tini` as PID 1.
- The Claude Code binary is downloaded from Anthropic's release bucket and its
  SHA-256 checksum is verified against the release manifest at build time.
- The Cursor CLI package is downloaded over HTTPS from Cursor's download
  server, the same package `https://cursor.com/install` uses. Cursor publishes
  no checksums for it; pin `CURSOR_VERSION` for repeatable builds, or build
  with `CURSOR_VERSION=none` if you do not use Cursor.

## Recommendations

- Serve the server over HTTPS through a reverse proxy.
- Use a long, random `ADMIN_PASSWORD`.
- Keep `/admin` and `/login` on your home network or VPN if you can; apps only need `/v1`.
- Give each app its own key and delete keys you no longer use.
- `ANTHROPIC_API_KEY` is never passed to the CLI, so requests always use your subscriptions.
- Keep subscriptions to yourself; the project is meant for personal use. Only
  connect a work or someone else's subscription if its owner and their
  policies allow it, and check each provider's terms.

## Reporting a Problem

Open an issue on GitHub without including tokens, keys or logs that contain
personal data.
