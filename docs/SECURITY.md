# Security

How claude-max-to-api protects your subscription, your keys and your data, and
what to do when you deploy it. For setup, see [SETUP.md](SETUP.md).

## What Is Protected

| Asset                    | Where it lives                                  | Protection                                                        |
| ------------------------ | ----------------------------------------------- | ----------------------------------------------------------------- |
| Claude OAuth token       | `DATA_DIR/claude-oauth-token`                   | File mode `0600`; only a redacted hint is ever shown              |
| Virtual API keys         | `DATA_DIR/keys.json`                            | Only SHA-256 hashes are stored; the secret is shown once          |
| Admin sessions           | `cmta_admin` cookie                             | HMAC-signed, `HttpOnly`, `SameSite=Strict`, `Secure` over HTTPS, 7 days |
| Session signing secret   | `DATA_DIR/session-secret`                       | Random per installation, mixed with `ADMIN_PASSWORD`; mode `0600` |
| Prompts, photos, replies | Memory only                                     | Not written to disk or logs; sessions are not persisted by the CLI |

## API Clients

- Every request needs a virtual key; disabled or deleted keys stop working at once.
- Claude Code's built-in tools (shell, files, web) are disabled and no settings
  files are loaded, so a prompt cannot make Claude run commands or read files
  in the container.
- Client tools are only described to Claude through a loopback endpoint with a
  random path per request. Calls are returned to the client; the server never
  executes them.
- Request bodies are limited by `MAX_BODY_MB`, processes by `MAX_CONCURRENT` and
  time by `REQUEST_TIMEOUT`.

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

## Logs

Logs never contain tokens, keys, the admin password, sign-in codes, prompts or
responses. A redacting handler drops sensitive fields and masks anything that
looks like an `sk-ant-` or `sk-cmta-` secret, including inside error messages.

## Container

- Runs as the unprivileged user `app` (uid 1000) with `tini` as PID 1.
- The Claude Code binary is downloaded from Anthropic's release bucket and its
  SHA-256 checksum is verified against the release manifest at build time.

## Recommendations

- Serve the server over HTTPS through a reverse proxy.
- Use a long, random `ADMIN_PASSWORD`.
- Keep `/admin` and `/login` on your home network or VPN if you can; apps only need `/v1`.
- Give each app its own key and delete keys you no longer use.
- Do not set `ANTHROPIC_API_KEY`: the CLI would bill that key instead of your subscription.
- Keep the subscription to yourself; the project is meant for personal use.

## Reporting a Problem

Open an issue on GitHub without including tokens, keys or logs that contain
personal data.
