# Setup

How to run CreditAPI, connect your Claude and Cursor subscriptions and use them
from any OpenAI-compatible app. For a short overview, see
[README.md](../README.md); for development, see [DEV.md](DEV.md).

## Requirements

- At least one subscription: Claude Pro or Max, and/or a Cursor plan
- A server or home lab machine with Docker and Docker Compose
- About 1 GB of free memory (each request in progress runs one CLI process; a
  Claude process uses roughly 250-350 MB, a Cursor process has not been
  measured)
- Optional but recommended: a domain name and HTTPS through a reverse proxy

## Install

```bash
git clone https://github.com/atb00ker/CreditAPI
cd CreditAPI
cp .env.example .env
```

Edit `.env` and set at least:

```bash
ADMIN_PASSWORD=a-long-random-password      # protects /admin and /login
PUBLIC_URL=https://ai.example.com          # the address you open in a browser
```

Then start it:

```bash
docker compose up -d --build
docker compose logs -f
```

The server listens on port 8080. `docker-compose.yml` publishes it on
`127.0.0.1` only; change the `ports` line to `"8080:8080"` to reach it from
other devices on your network, such as your phone. All state (API keys, subscriptions, saved
logins, CLI files) lives in the `creditapi-data` volume. Every setting is
described in [ENVIRONMENT.md](ENVIRONMENT.md).

The image contains both the Claude Code CLI and the Cursor CLI. If you only use
Claude, build without Cursor to save about 540 MB:
`docker compose build --build-arg CURSOR_VERSION=none`.

## Subscriptions and Keys

A **subscription** is one AI account that answers requests, for example
`Claude (Personal)`, `Cursor (Personal)`, `Claude (Work)` or `Cursor (Friend)`.
A **key** is what an app uses to call the API. Each key may use any set of
subscriptions, and each subscription may serve any set of keys:

| Subscription | Keys that may use it |
| --- | --- |
| Claude (Personal) | app-1, app-2, app-3 |
| Cursor (Personal) | app-1, app-4, app-5 |
| Claude (Work) | app-4, app-5 |
| Cursor (Friend) | none yet |

The first start creates one subscription called `Claude` (and one called
`Cursor` when `CURSOR_API_KEY` is set). Add more on `/admin` under
**Subscriptions**: give it a name, pick Claude or Cursor and select **Add
subscription**. Then tick which keys may use it in the **Access** grid.

Only connect a subscription you are allowed to use this way. A work or
someone else's account needs its owner's permission and must follow their
policies and the provider's terms.

## Connect a Claude Subscription

1. Open `https://your-server/login` (a phone is fine) and enter the admin
   password. Pick the subscription at the top, or use the **Sign-in** button
   next to it on `/admin`.
2. The page first checks whether the current login works. If it does, it says
   you are all set and there is nothing else to do.
3. If a sign-in is needed, it starts one by itself. Tap **Open Claude sign-in**
   (a new tab), log in with the Claude account for this subscription and
   approve.
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
on `/login`. For the first `Claude` subscription you can also set
`CLAUDE_CODE_OAUTH_TOKEN` in `.env`; a login done on `/login` always takes
precedence over it.

## Connect a Cursor Subscription

1. Open `/login` and pick the Cursor subscription.
2. If a sign-in is needed, tap **Open Cursor sign-in** and approve with the
   Cursor account for this subscription.
3. Come back to the `/login` tab. There is no code to paste: the page notices
   by itself when the sign-in went through and confirms it with a test prompt.

The Cursor CLI keeps this sign-in in the subscription's own folder in the data
volume. **Sign out** on `/login` runs `cursor-agent logout` for it.

Instead of signing in through the browser you can paste a Cursor API key
(created in your Cursor dashboard) under **Use a Cursor API key instead?**. For
the first `Cursor` subscription you can also set `CURSOR_API_KEY` in `.env`.

Which models a Cursor subscription offers is set with `CURSOR_MODELS` (default
`auto`); see [ENVIRONMENT.md](ENVIRONMENT.md). The Cursor CLI cannot switch its
built-in tools off, so read [SECURITY.md](SECURITY.md#cursor) before connecting
one.

## Create an API Key

Open `/admin`, enter a name (for example `my-app`) and select **Create key**.
Copy the `sk-credit-...` key right away; it is shown only once. A new key may
use every subscription; change that in the **Access** grid. Give every app its
own key so you can see its usage and revoke it on its own.

## Connect an App

Any app or library that supports OpenAI, an "OpenAI compatible" provider, or
Anthropic with a custom base URL can use CreditAPI.

For an **Anthropic** provider, set the base URL to `https://your-server` (some
apps want `https://your-server/v1`, or a separate URL: then use
`https://your-server/anthropic`) and the API key to a key from `/admin`.
Model names such as `claude-haiku-4-5`, `haiku` or `sonnet` work. Details are
in [API.md](API.md#anthropic-messages-api).

For an **OpenAI** provider, use:

| Setting | Value |
| --- | --- |
| Base URL | `https://your-server/v1` (some apps want it without `/v1`; both work) |
| API key | the key from `/admin` |
| Model | for example `sonnet`, `opus`, `haiku` (Claude) or `auto` (Cursor) |

If the app supports image input, enable it for the same provider so photos
work. Apps that run in Docker on the same host can reach CreditAPI by container
name: put both on one network (uncomment the `networks` block in
`docker-compose.yml`) and use `http://creditapi:8080/v1`. Some apps block
private or LAN addresses until you allow them in their settings.

From code, use any OpenAI client:

```python
from openai import OpenAI

client = OpenAI(base_url="https://your-server/v1", api_key="sk-credit-...")
reply = client.chat.completions.create(
    model="sonnet",
    messages=[{"role": "user", "content": "Summarize the plot of Hamlet in two sentences."}],
)
print(reply.choices[0].message.content)
```

```bash
curl https://your-server/v1/chat/completions \
  -H "Authorization: Bearer sk-credit-..." -H "Content-Type: application/json" \
  -d '{"model":"haiku","messages":[{"role":"user","content":"Hi!"}]}'
```

## Which Subscription Answers

For each request, CreditAPI looks at the subscriptions the key may use, from
the top of the list on `/admin` down (use **Move up** and **Move down** to
change the order):

1. Subscriptions whose provider offers the requested model come first. `sonnet`
   goes to a Claude subscription, `auto` or another `CURSOR_MODELS` name to a
   Cursor one.
2. The key's other subscriptions follow, each with its own default model
   (`DEFAULT_MODEL` or `CURSOR_DEFAULT_MODEL`). Set `STRICT_MODELS=true` to
   refuse unknown model names instead.
3. If a subscription is signed out, at its usage limit, overloaded or fails
   before it answered anything, the next one is tried.

To use one particular subscription, put its short name in front of the model:
`claude-work/sonnet` or `cursor-friend/auto`. The short name is shown on
`/admin` and is made from the subscription's name. Such a request never falls
back to another subscription. `GET /v1/models` lists every name a key can use.

## When a Login Expires

Apps cannot open this server's login page by themselves. Instead they receive
an error that explains what to do:

```text
HTTP/1.1 401 Unauthorized

{"error": {"type": "authentication_error", "code": "subscription_login_required",
  "message": "Sign-in required: Claude (Personal) is signed out or its credential expired. Sign in at https://ai.example.com/login?subscription=1a2b3c4d5e6f and then try again."}}
```

You only get this error when none of the key's subscriptions could answer; as
long as another one works, requests keep flowing and only `/admin` shows the
signed-out subscription. If the app shows provider errors (in a chat, for
example) the link is right there and opens `/login` on the right subscription;
otherwise open `/login` yourself. A few details:

- The link uses `PUBLIC_URL`. Without it, it uses the address the app called,
  which inside Docker is an internal name your phone cannot open.
- The error is a 401 so OpenAI client libraries report it at once instead of retrying.
- Until a login is fixed, that subscription is skipped without starting its
  CLI (for up to 5 minutes at a time).
- `GET /healthz` returns `"logins_ok": false` when any enabled subscription
  needs a sign-in, and each subscription's `login` state, so an uptime monitor
  can alert you.

## Running Behind a Proxy (Optional)

CreditAPI is a plain HTTP server and works on its own, on a laptop or a home
server. If you put it behind a reverse proxy for HTTPS, such as Nginx or
Caddy, see [examples/nginx.conf](../examples/nginx.conf). Two Nginx defaults
need changing: the 1 MB request body limit (photos are larger) and response
buffering (it holds back streamed answers).

- Set `PUBLIC_URL` to the address you open in the browser. Login links use it,
  and an `https://` address marks the admin cookie `Secure`.
- Forwarding headers (`X-Forwarded-For`, `-Proto`, `-Host`) are ignored unless
  the request comes from an address in `TRUSTED_PROXIES`, because any client
  can send them. Set it so the admin sign-in lockout counts real client
  addresses instead of the proxy's.
- If you can, keep `/admin` and `/login` reachable only from your home network
  or VPN; apps only need `/v1`.

## Updating

```bash
git pull
docker compose up -d --build
```

The image installs pinned versions of the Claude Code CLI and the Cursor CLI
(see the `ARG` lines in the `Dockerfile`; Dependabot proposes base image
updates). To build other versions, pass `--build-arg CLAUDE_VERSION=...` and
`--build-arg CURSOR_VERSION=...`; a Cursor version also needs its checksums in
`scripts/cursor-checksums.txt`, or `--build-arg CURSOR_ALLOW_UNVERIFIED=1`.
Your keys, subscriptions and logins survive updates in the data volume. An
install from before subscriptions existed is moved over on the first start:
its saved Claude login becomes the `Claude` subscription and every existing key
may use it.

## Sizing

| Setting | Default | Effect |
| --- | --- | --- |
| `MAX_CONCURRENT` | `2` | Requests answered at the same time, across all subscriptions; each uses roughly 250-350 MB |
| `MAX_PENDING` | `8` | Requests running or waiting; more get `429 server_busy` at once |
| `QUEUE_TIMEOUT` | `2m` | How long extra requests wait before `429 server_busy` |
| `mem_limit` | `1536m` | Container memory limit in `docker-compose.yml` |

The server itself uses about 6-15 MB. Each Claude request adds 1-2 seconds of
start-up time for the CLI process; Cursor has not been measured.

## Troubleshooting

| Symptom | What to do |
| --- | --- |
| `401 invalid_api_key` | The app's key is wrong, disabled or deleted. Check `/admin`. |
| `401 subscription_login_required` | Open the link in the message (or `/login`) and sign in again. |
| `403 no_subscription` | The key may not use any (enabled) subscription, or not the one named in the model. Tick it under **Access** on `/admin`. |
| `404 model_not_found` | `STRICT_MODELS` is on and no subscription of the key offers that model. |
| `429 rate_limit_exceeded` | Every subscription the key may use is at its usage limit; `/admin` shows when Claude limits reset. |
| `429 server_busy` | All slots are busy; raise `MAX_CONCURRENT` if you have the memory. |
| No **Paste** button on `/login` | The page is not on HTTPS; paste into the box instead. |
| The app refuses a private or LAN URL | Many apps block private addresses by default; allow them in the app's settings. |
| Photos fail with HEIC or AVIF errors | Send JPEG; most phones can share photos as JPEG. |

Logs are JSON lines on stdout (`docker compose logs`); each request carries a
`request_id` that is also returned in the `X-Request-Id` header, and the
`subscription` that answered it.
