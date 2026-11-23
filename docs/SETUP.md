# Setup

How to run claude-max-to-api, connect your Claude subscription and use it from
SparkyFitness or any other OpenAI-compatible app. For a short overview, see
[README.md](../README.md); for development, see [DEV.md](DEV.md).

## Requirements

- A Claude Pro or Max subscription
- A server or home lab machine with Docker and Docker Compose
- About 1 GB of free memory (each request in progress runs one `claude` process of roughly 250-350 MB)
- Optional but recommended: a domain name and HTTPS through a reverse proxy

## Install

```bash
git clone https://github.com/atb00ker/claude-max-to-api
cd claude-max-to-api
cp .env.example .env
```

Edit `.env` and set at least:

```bash
ADMIN_PASSWORD=a-long-random-password      # protects /admin and /login
PUBLIC_URL=https://claude.example.com      # the address you open in a browser
```

Then start it:

```bash
docker compose up -d --build
docker compose logs -f
```

The server listens on port 8080. All state (API keys, the saved Claude login,
CLI files) lives in the `claude-data` volume. Every setting is described in
[ENVIRONMENT.md](ENVIRONMENT.md).

## Connect Your Claude Subscription

1. Open `https://your-server/login` (a phone is fine) and enter the admin password.
2. The page first checks whether the current login works. If it does, it says
   you are all set and there is nothing else to do.
3. If a sign-in is needed, it starts one by itself. Tap **Open Claude sign-in**
   (a new tab), log in with your Claude account and approve.
4. Claude shows an authentication code. Come back to the `/login` tab, tap
   **Paste** (or paste into the box) and **Finish sign-in**. The page sends a
   one-word test prompt to confirm it works.

The token is valid for about a year and is saved in the data volume. Reloading
`/login` in the middle of a sign-in resumes it, so a code you already copied
stays valid. The **Paste** button needs the page to be served over HTTPS.

Claude's sign-in for the CLI finishes on Anthropic's own page, which shows the
code. That is why you paste it instead of being redirected back.

**Already have a token?** Run `claude setup-token` on any computer with a
browser and paste the `sk-ant-oat01-...` token under **Already have a token?**
on `/login`, or set `CLAUDE_CODE_OAUTH_TOKEN` in `.env`. A login done on
`/login` always takes precedence over the environment variable.

## Create an API Key

Open `/admin`, enter a name (for example `sparkyfitness`) and select **Create
key**. Copy the `sk-cmta-...` key right away; it is shown only once. Give every
app its own key so you can see its usage and revoke it on its own.

## Connect SparkyFitness

In SparkyFitness open the AI settings and add a service:

| Field            | Value                                                  |
| ---------------- | ------------------------------------------------------ |
| Service type     | OpenAI Compatible                                      |
| Base URL         | `http://claude-max-to-api:8080/v1` (must end in `/v1`) |
| API key          | the key from `/admin`                                  |
| Use custom model | on, model `sonnet` (or `opus`, `haiku`)                |

Select the same service for **Vision** so food photos and nutrition labels work.

If both apps run in Docker, put them on the same network so SparkyFitness can
reach the container by name. Uncomment the `networks` block in
`docker-compose.yml` and set the name of SparkyFitness's network. SparkyFitness
only allows AI URLs on private addresses for admins, or when
`ALLOW_PRIVATE_NETWORK_AI=true` is set on SparkyFitness.

## Other Apps

Any OpenAI client works. Use the base URL `https://your-server/v1` and a key
from `/admin`:

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
  -d '{"model":"haiku","messages":[{"role":"user","content":"Hi!"}]}'
```

`opus`, `sonnet` and `haiku` always mean the newest model of each family.
Unknown names such as `gpt-4o` use `DEFAULT_MODEL`. What is supported is listed
in [API.md](API.md).

## When the Login Expires

Apps cannot open this server's login page by themselves. Instead they receive
an error that explains what to do:

```text
HTTP/1.1 401 Unauthorized

{"error": {"type": "authentication_error", "code": "claude_login_required",
  "message": "Claude login required: this server's Claude subscription is signed out or its token expired. Sign in at https://claude.example.com/login and then try again."}}
```

If the app shows provider errors (in a chat, for example) the link is right
there; otherwise open `/login` yourself. A few details:

- The link uses `PUBLIC_URL`. Without it, it uses the address the app called,
  which inside Docker is an internal name your phone cannot open.
- The error is a 401 so OpenAI client libraries report it at once instead of retrying.
- Until the login is fixed, requests fail immediately without starting `claude`
  (for up to 5 minutes at a time).
- `GET /healthz` returns `"claude_login": "valid" | "login_required" | "unknown"`,
  so an uptime monitor can alert you.

## HTTPS and Reverse Proxy

Put the server behind HTTPS before exposing it to the internet. With Caddy:

```text
claude.example.com {
  reverse_proxy claude-max-to-api:8080
}
```

The admin cookie is marked `Secure` when requests arrive over HTTPS
(`X-Forwarded-Proto` is respected). If you can, keep `/admin` and `/login`
reachable only from your home network or VPN; apps only need `/v1`.

## Updating

```bash
git pull
docker compose up -d --build
```

The image installs the latest `stable` Claude Code CLI at build time. To pin a
version, build with `docker compose build --build-arg CLAUDE_VERSION=2.1.323`.
Your keys and login survive updates in the data volume.

## Sizing

| Setting           | Default | Effect                                                         |
| ----------------- | ------- | -------------------------------------------------------------- |
| `MAX_CONCURRENT`  | `2`     | Requests answered at the same time; each uses roughly 250-350 MB |
| `QUEUE_TIMEOUT`   | `2m`    | How long extra requests wait before `429 server_busy`          |
| `mem_limit`       | `1536m` | Container memory limit in `docker-compose.yml`                 |

The server itself uses about 6-15 MB. Each request adds 1-2 seconds of start-up
time for the `claude` process.

## Troubleshooting

| Symptom                                   | What to do                                                                 |
| ----------------------------------------- | -------------------------------------------------------------------------- |
| `401 invalid_api_key`                     | The app's key is wrong, disabled or deleted. Check `/admin`.               |
| `401 claude_login_required`               | Open `/login` and sign in again.                                           |
| `429 rate_limit_exceeded`                 | Your subscription's usage limit is reached; `/admin` shows when it resets. |
| `429 server_busy`                         | All slots are busy; raise `MAX_CONCURRENT` if you have the memory.         |
| No **Paste** button on `/login`           | The page is not on HTTPS; paste into the box instead.                      |
| SparkyFitness says the URL is not allowed | Set `ALLOW_PRIVATE_NETWORK_AI=true` on SparkyFitness or configure it as an admin. |
| Photos fail with HEIC or AVIF errors      | Send JPEG; most phones can share photos as JPEG.                           |

Logs are JSON lines on stdout (`docker compose logs`); each request carries a
`request_id` that is also returned in the `X-Request-Id` header.
