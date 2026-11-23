# CreditAPI Code Review: Security, Quality and Maintainability

Reviewed commit: `be2d525` (main, after PR #3). Date: 2026-11-23.

This file is a work order. It lists what should change, why, where, and how to
know it is done. It is written for a model or person who will implement the
changes without the context of the review.

## How to Use This File

1. Read "Ground Rules" and "What Was Checked" first.
2. Work in the order given in "Suggested Batches". Each batch is one pull
   request. Do not mix batches.
3. Every item has an ID, a severity and an effort estimate
   (S: under an hour, M: a few hours, L: a day or more).
4. Items marked **OWNER** need a decision or a real account and must not be
   guessed. Skip them and list them in the PR description.
5. When all batches are merged, delete this file (it describes weaknesses and
   should not stay in a public repository).

## Ground Rules for the Implementer

- Follow `CLAUDE.md`: new behaviour needs a Go test; API-visible behaviour also
  needs a Bruno request; new environment variables go in `OwnEnvVars`,
  `.env.example` and `docs/ENVIRONMENT.md`; errors use the OpenAI envelope and
  are documented in `docs/API.md`; no em-dashes in comments or docs.
- The Bruno `.bru` files are the source of truth. They were generated once by
  a script that is not in the repository. Edit the `.bru` files by hand.
- Run `make ci` and `make e2e-ci` before every push. Both must pass.
- Do not change the fake CLIs to make a server change pass unless the item
  says so. The fakes define the contract.
- The Cursor backend was written from Cursor's documentation. Nobody has run
  the real `cursor-agent` against it. Do not claim real-CLI behaviour you
  cannot test; say what was and was not verified.
- Keep changes small and behaviour-preserving unless the item says otherwise.
  Refactors (the QUAL items) must not change responses, status codes or logs.
- Do not add dependencies without a reason stated in the PR.

## What Was Checked

| Check | Result |
| --- | --- |
| `go vet ./...` | Clean |
| `staticcheck ./...` | Clean |
| `go test -race ./...`, Bruno (102 requests) | Pass |
| `govulncheck ./...` | 18 reachable standard library advisories, all fixed by Go 1.26.11 (OPS-1) |
| `gosec` on application code | 18 findings: 7 unchecked errors (QUAL-7), the rest expected for this design (subprocess and file paths from configuration) |
| Coverage per package | admin 72.7%, keys 78.0%, openai 81.0%, claude 83.4%, agent 85.4%, cursor 85.9%, server 86.2%, subscriptions 89.8%, logging 90.7%, config 91.6% |
| Manual review | All non-test Go code, the two web pages and their script, Dockerfile, compose file, install scripts, CI workflow |
| Experiment: oversized image | A 17.6 KB PNG made the server allocate 722 MB (SEC-1) |
| Fuzzing, 20 s each | EXIF orientation parser and stop-sequence filter: no panics, no wrong output |

Not checked: the real Cursor CLI, the real Claude CLI under these changes, a
deployed instance behind a reverse proxy, the published Docker image contents.

## Things That Are Already Good (Do Not "Fix")

- Claude Code runs with built-in tools off and no settings sources.
- Each subscription has its own `HOME` and receives only its own credential.
- Virtual keys are 256-bit random values stored as SHA-256 hashes.
- The image download guard checks the resolved address at connect time, so DNS
  rebinding does not bypass it.
- The admin pages use a strict Content Security Policy and build the DOM with
  `textContent` and `setAttribute`. No `innerHTML`. No XSS sink was found.
- Cookie is `HttpOnly`, `SameSite=Strict`; state-changing cookie requests are
  origin-checked.
- Subscription IDs are validated before they are used in file paths.
- `EnvCredential` on a subscription is only set by first-start migration, never
  through the API. Keep it that way: making it settable would let the admin API
  read arbitrary environment variables.

## Summary of Findings

| ID | Severity | Effort | Title |
| --- | --- | --- | --- |
| SEC-1 | High | S | A tiny image can exhaust server memory |
| OPS-1 | High | S | Go toolchain has 18 reachable known vulnerabilities |
| SEC-2 | Medium | M | Unbounded memory before a request is queued |
| SEC-3 | Medium | M | Cursor runs can read other requests' temporary files |
| SEC-4 | Medium | S | The example admin password is accepted |
| SEC-5 | Medium | M | Login lockout and forwarded headers are not proxy-aware |
| BUG-1 | Medium | S | Streaming requests that time out end without an error |
| SEC-6 | Low | S | `/healthz` shows subscription names without authentication |
| SEC-7 | Low | S | Model names reach the CLIs without validation |
| SEC-8 | Low | S | No read timeout: slow request bodies hold connections open |
| SEC-9 | Low | S | Internal error text is returned to API clients |
| SEC-10 | Low | S | Log redaction misses Cursor keys and the admin password |
| SEC-11 | Low | S | Admin sessions cannot be revoked |
| SEC-12 | Low | S | Message text can forge conversation structure |
| SEC-13 | Low | S | Image download guard misses some reserved address ranges |
| SEC-14 | Low | S | Sign-in link is not restricted to the provider's hosts |
| SEC-15 | Low | S | Container runs with default privileges |
| BUG-2 | Low | S | JSON mode returns empty content when the reply is not JSON |
| BUG-3 | Low | S | A crashed Claude process can look like a complete answer |
| BUG-4 | Low | M | Rate-limited subscriptions are retried on every request |
| BUG-5 | Low | S | Shutdown can leave CLI processes running |
| BUG-6 | Low | S | Configuration accepts values that break the server |
| BUG-7 | Low | S | Key and grant files can drift apart |
| BUG-8 | Low | S | Small correctness issues (three) |
| OPS-2 | Medium | S | CI does not scan for vulnerabilities or lint |
| OPS-3 | Low | S | No automated dependency updates; actions not pinned |
| OPS-4 | Low | M | Image build is not reproducible or fully verified |
| QUAL-1 | n/a | M | `openai/handler.go` does too much |
| QUAL-2 | n/a | M | Two very long functions in the backends |
| QUAL-3 | n/a | S | Error kinds and provider names are bare strings |
| QUAL-4 | n/a | S | Duplicated helpers in the wrong packages |
| QUAL-5 | n/a | M | Admin API responses are untyped maps |
| QUAL-6 | n/a | M | Only one sign-in can run at a time |
| QUAL-7 | n/a | S | No lint configuration; seven unchecked errors |
| QUAL-8 | n/a | S | No panic recovery; server errors bypass JSON logs |
| TEST-1 | n/a | M | Coverage gaps and missing fuzz tests |
| DOC-1 | n/a | S | Documentation corrections |

## Security

### SEC-1 (High, S): A tiny image can exhaust server memory

- **Where:** `src/internal/openai/images.go:204` (`image.Decode`) in `normalize`.
- **Problem:** `normalize` reads the image header, then decodes the full image
  whenever the dimensions exceed `IMAGE_MAX_DIMENSION`. It never checks the
  pixel count. A PNG that declares huge dimensions compresses to almost
  nothing. Measured: a 17.6 KB, 12000x12000 PNG allocated 722 MB. Any holder
  of a virtual key can crash the container with one request, or with a URL if
  `ALLOW_IMAGE_URLS` is on. Decoding happens before a slot is acquired, so
  several such requests run in parallel.
- **Fix:**
  1. After `image.DecodeConfig`, reject when width or height is not positive
     or `int64(w)*int64(h)` exceeds a limit. New variable `IMAGE_MAX_PIXELS`,
     default `40000000`. Return a 400 through the existing `requestError` path
     with a message that names the limit.
  2. Add a package-level semaphore so at most two images are decoded at once.
     It must respect the request context.
- **Tests:** a unit test that builds a paletted 8000x8000 PNG and asserts the
  error and that `runtime.MemStats.TotalAlloc` grew by less than 50 MB during
  `normalize`; a config test for the new variable; one Bruno request is not
  needed (the payload would be awkward), but add the error to `docs/API.md`.
- **Done when:** the test above passes and normal photos still resize (existing
  image tests pass).

### SEC-2 (Medium, M): Unbounded memory before a request is queued

- **Where:** `src/internal/openai/handler.go:119` (body read, up to
  `MAX_BODY_MB`, default 50), `:160` (message conversion and image work),
  `:193` (slot acquired only here).
- **Problem:** `MAX_CONCURRENT` limits CLI processes, not requests in
  preparation. Each waiting request holds its body, the parsed JSON and decoded
  images for up to `QUEUE_TIMEOUT` (2 minutes). A key holder can open many
  large requests and exhaust memory.
- **Fix:** add an admission limit acquired right after authentication and
  before the body is read. New variable `MAX_PENDING` (requests admitted at
  once, running plus waiting), default `MAX_CONCURRENT * 4`, minimum
  `MAX_CONCURRENT`. When full, answer `429 server_busy` at once. Release it
  when the handler returns. Keep the existing slot logic unchanged.
- **Tests:** integration test with `MAX_CONCURRENT=1`, `MAX_PENDING=2` and
  three `[fake:slow]` requests: the third gets 429 immediately. Update the
  concurrency Bruno request only if its expectations change.
- **Done when:** memory use is bounded by `MAX_PENDING * MAX_BODY_MB` plus
  decode buffers, and the limit is documented in `docs/ENVIRONMENT.md` and the
  sizing table in `docs/SETUP.md`.

### SEC-3 (Medium, M): Cursor runs can read other requests' temporary files

- **Where:** `src/internal/agent/cursor/cursor.go:290` (workspace in the
  system temp directory), `:130` (`permissions`), and
  `src/internal/agent/claude/claude.go:180` (Claude's per-run temp directory,
  which holds the system prompt and the MCP bridge URL).
- **Problem:** the Cursor CLI cannot turn its file tools off. The deny rules
  cover the data directory and system paths but not the temp directory, where
  every concurrent request (Cursor and Claude) keeps its prompt, images and
  bridge address. A prompt-injected Cursor agent could read another request's
  conversation. Cursor's documentation states that deny rules take precedence
  over allow rules, so denying the whole temp directory would also deny the
  run's own workspace.
- **Fix (do all of 1 to 4):**
  1. Give each provider its own private run root with mode `0700`, created at
     start: `<os.TempDir()>/creditapi-<uid>/claude` and `.../cursor`. Create
     per-run directories inside them.
  2. Add the Claude run root to the Cursor deny list
     (`Read(<claude root>/**)`).
  3. Set `TMPDIR` for the Cursor process to its own workspace.
  4. Make the fake Cursor CLI also reject `--yolo` (it is an alias of
     `--force`), and assert in a test that the deny list contains the Claude
     run root.
  5. **OWNER:** the Cursor CLI documents a `--sandbox <enabled|disabled>`
     option. Test with a real account whether `--sandbox enabled` works in the
     Docker image in headless mode, and whether it confines file reads to the
     workspace. If it does, pass it always and document it. Do not add the flag
     without that test: it may fail inside containers.
- **Residual risk to document in `docs/SECURITY.md`:** two Cursor requests
  running at the same time can still read each other's workspace, because a
  rule cannot allow one sibling directory and deny the rest. Only an OS-level
  sandbox (item 5) closes this.

### SEC-4 (Medium, S): The example admin password is accepted

- **Where:** `.env.example:4` ships `ADMIN_PASSWORD=change-me-to-something-long`;
  `src/internal/config/config.go:115` only checks length.
- **Problem:** someone who copies the example file and forgets to edit it runs
  with a password that is published in the repository.
- **Fix:** refuse to start when the password equals the `.env.example`
  placeholder (compare case-insensitively) and say why in the error. Log a
  warning when the password is shorter than 12 characters. Keep the minimum at
  8. The Bruno test password in `.env.bruno.example` stays allowed; it is only
  used with the fake CLIs.
- **Tests:** config test for the placeholder.

### SEC-5 (Medium, M): Login lockout and forwarded headers are not proxy-aware

- **Where:** `src/internal/admin/admin.go:159` (`clientIP`), `:155`
  (`secureRequest`), `:168` (`sameOrigin`), `:416` (`limiter`);
  `src/internal/openai/handler.go:564` (`loginURL`).
- **Problems:**
  1. The lockout counts failures per `RemoteAddr`. Behind a reverse proxy
     (the documented setup) every visitor has the proxy's address, so ten wrong
     passwords from anyone lock the real admin out for ten minutes, and repeat.
  2. `X-Forwarded-Proto` and `X-Forwarded-Host` are trusted from any client.
  3. The limiter map only shrinks when the same address returns; many
     addresses grow it without bound. IPv6 clients get a fresh bucket per
     address.
- **Fix:**
  1. New variable `TRUSTED_PROXIES` (comma-separated CIDRs or addresses,
     default empty). Forwarded headers are honoured only when `RemoteAddr` is
     inside it. The client address is the right-most `X-Forwarded-For` entry
     that is not a trusted proxy. Put this in one small helper package used by
     both `admin` and `openai`.
  2. When `PUBLIC_URL` starts with `https://`, always set the cookie `Secure`
     flag, and let `sameOrigin` also accept an `Origin` whose host equals the
     `PUBLIC_URL` host. This keeps the common setup working without
     `TRUSTED_PROXIES`.
  3. Key the limiter by /64 for IPv6, cap the map (for example 10,000
     entries, drop the oldest), and prune expired entries on a timer.
- **Tests:** table tests for the helper (trusted and untrusted proxy, spoofed
  header, IPv6); limiter tests for /64 grouping and the cap; an integration
  test that a spoofed `X-Forwarded-For` does not change the bucket by default.
- **Docs:** `docs/ENVIRONMENT.md`, the reverse proxy section of
  `docs/SETUP.md`, `docs/SECURITY.md`.

### SEC-6 (Low, S): `/healthz` shows subscription names without authentication

- **Where:** `src/internal/server/server.go:108`.
- **Problem:** anyone who can reach the server sees each subscription's ID,
  name (for example "Cursor (Friend)"), provider and login state.
- **Fix:** without authentication return only `ok`, `logins_ok` and counts
  (`subscriptions`, `need_login`). Return the per-subscription list only when
  the request carries a valid admin session or bearer password. The Docker
  health check needs only the status code.
- **Tests and docs:** update the server test, the integration tests that read
  `subscriptions` from `/healthz` (send the admin bearer), the two Bruno health
  requests, `docs/API.md` and `docs/SETUP.md`.

### SEC-7 (Low, S): Model names reach the CLIs without validation

- **Where:** `src/internal/subscriptions/manager.go:282` (pinned path passes
  the text after the slash straight through),
  `src/internal/agent/claude/claude.go:62` (`Accepts` takes anything starting
  with `claude-` or containing `[1m]`).
- **Problem:** the value is one `argv` element after `--model`, so there is no
  shell injection. But a value that starts with `-` or contains odd characters
  depends on each CLI's option parser, and nonsense values waste a process and
  return confusing upstream errors.
- **Fix:** validate the requested model once, in `Candidates`, against
  `^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,127}$` (after removing a subscription
  prefix). Reject with a new `agent.Error` kind mapped to
  `400 invalid_request_error`, code `invalid_model`. In pinned mode, when the
  model is not empty and the backend does not accept it, return
  `404 model_not_found`.
- **Tests:** routing tests for `-x`, a 200-character name, `a b`, and a pinned
  unknown model; one Bruno request; `docs/API.md`.

### SEC-8 (Low, S): No read timeout: slow request bodies hold connections open

- **Where:** `src/internal/server/server.go:182` (`http.Server` has only
  `ReadHeaderTimeout` and `IdleTimeout`); `src/internal/admin/admin.go:391`
  (`readJSON` runs before authentication on `POST /admin/api/session`).
- **Fix:** set `ReadTimeout: 5 * time.Minute` and `MaxHeaderBytes: 64 << 10` on
  the server. Do not set `WriteTimeout` (it would cut streaming responses). In
  `readJSON`, set a 15 second read deadline with
  `http.NewResponseController(w).SetReadDeadline` (pass `w` in), and return 413
  when the body exceeds the 64 KiB limit instead of silently truncating.
- **Tests:** a test that a body sent after the deadline fails; existing
  streaming tests must still pass.

### SEC-9 (Low, S): Internal error text is returned to API clients

- **Where:** `src/internal/openai/handler.go:166` and `:600` return
  `err.Error()`; upstream errors carry up to 500 characters of CLI stderr
  (`src/internal/agent/types.go:123`).
- **Problem:** file paths, temp directory names and whatever a CLI prints can
  reach an app's user.
- **Fix:** for `internal_error` return "internal error; see the server log
  (request <id>)" and log the detail. For upstream errors keep the message but
  pass it through the log redactor (SEC-10) and replace the data directory and
  run-root paths with `<data>` and `<tmp>`.
- **Tests:** unit test for the sanitizer; the existing `[fake:crash]` tests
  still see "simulated crash".

### SEC-10 (Low, S): Log redaction misses Cursor keys and the admin password

- **Where:** `src/internal/logging/redact.go:15` matches only `sk-ant-`,
  `sk-credit-` and `sk-cmta-` prefixes.
- **Fix:** add a registry of live secret values:
  `logging.RegisterSecret(value string)` (ignore values shorter than 8
  characters) and have the handler replace exact matches with `[redacted]`.
  Register `ADMIN_PASSWORD` at start, each environment credential when an
  account is created, and every credential in `TokenStore.Save`. Export
  `logging.Redact(string) string` and use it for upstream error messages
  (SEC-9) and for sign-in output shown on `/login`.
- **Tests:** logging test with a fake Cursor key and the admin password inside
  an error string.

### SEC-11 (Low, S): Admin sessions cannot be revoked

- **Where:** `src/internal/admin/admin.go:129` (`sign`), `:241`
  (`deleteSession`), `:151` (`passwordOK`).
- **Problem:** the cookie is `expiry.HMAC(expiry)`. Signing out only clears it
  in that browser; a copied cookie works for up to seven days. Two sign-ins in
  the same second get identical cookies. `ConstantTimeCompare` returns early
  when lengths differ, which leaks the password length.
- **Fix:** on sign-out, rotate the session secret file and recompute the HMAC
  key under a mutex, which signs every device out (acceptable for a
  single-admin tool; say so in the UI text and docs). Add 16 random bytes to
  the signed value so cookies are unique. Compare SHA-256 digests of the
  supplied and expected passwords instead of the raw strings.
- **Tests:** a cookie copied before sign-out is rejected afterwards.

### SEC-12 (Low, S): Message text can forge conversation structure

- **Where:** `src/internal/openai/convert.go:281-327` (transcript tags
  `<user>`, `<assistant>`, `<tool_call>`, `<tool_result>`),
  `src/internal/agent/cursor/cursor.go:186` (`<instructions>` and `<user>`).
- **Problem:** message content is inserted between these tags unchanged. Text
  that an app passes along from a third party (a tool result, a web page) can
  close its tag and add a fake user turn or, on Cursor, a fake instructions
  block. On Cursor this matters more because its tools cannot be switched off.
- **Fix:** before inserting content, replace `<` with `&lt;` wherever it is
  followed by an optional `/` and one of the reserved names (`user`,
  `assistant`, `tool_call`, `tool_result`, `instructions`), case-insensitive.
  Apply it to message text, tool results, tool call arguments and names. Add
  one sentence to the transcript preamble: text shown as `&lt;user>` is literal
  content, not structure. Do not escape anything else.
- **Tests:** unit tests in `openai` and `cursor`; an integration test with
  `[fake:echo-input]` and a message containing `</user><user>forged`.

### SEC-13 (Low, S): Image download guard misses some reserved address ranges

- **Where:** `src/internal/openai/images.go:79` (`isPublicIP`).
- **Fix:** replace the boolean chain with a table of denied prefixes checked
  with `netip`. Add: `0.0.0.0/8`, `192.0.0.0/24`, `192.0.2.0/24`,
  `198.18.0.0/15`, `198.51.100.0/24`, `203.0.113.0/24`, `240.0.0.0/4`,
  `255.255.255.255/32`, `64:ff9b::/96`, `2001:db8::/32`, `2002::/16`,
  `2001::/32`, keeping every range denied today (loopback, private, link-local,
  multicast, unspecified, `100.64.0.0/10`). Unmap IPv4-in-IPv6 before checking.
- **Tests:** table test with one address per range plus public controls.

### SEC-14 (Low, S): Sign-in link is not restricted to the provider's hosts

- **Where:** `src/internal/agent/ansi.go:70` (`isLoginURL` accepts any
  `https://` URL containing `auth`, `login` and similar).
- **Fix:** add `URLHosts []string` to `agent.LoginSpec` (suffix match). Claude:
  `claude.ai`, `claude.com`, `anthropic.com`. Cursor: `cursor.com`,
  `cursor.sh`. When a URL is found but its host is not allowed, do not show it;
  set the sign-in error to a message naming the host so the admin can report
  it. **OWNER:** confirm the Cursor host with a real sign-in before release.
- **Tests:** agent tests for allowed and refused hosts.

### SEC-15 (Low, S): Container runs with default privileges

- **Where:** `docker-compose.yml`.
- **Fix:** add `security_opt: ["no-new-privileges:true"]`, `cap_drop: [ALL]`
  and `pids_limit: 256`. Add a commented alternative port line
  `127.0.0.1:8080:8080` with a note to use it behind a reverse proxy on the
  same host. **OWNER:** `read_only: true` with a `tmpfs` on `/tmp` is worth
  trying but needs a real CLI run to confirm; leave it commented with that
  note.
- **Done when:** `docker compose config` is valid and the CI image job passes.

### Accepted Risks (No Change, Keep Documented)

- Credentials are stored unencrypted (mode `0600`) in the data volume. Treat
  volume backups as secrets.
- `ALLOW_IMAGE_URLS` defaults to on, so a key holder can make the server fetch
  public URLs.
- The admin password is accepted as a bearer token on the admin API.

## Bugs

### BUG-1 (Medium, S): Streaming requests that time out end without an error

- **Where:** `src/internal/openai/handler.go:526`.
- **Problem:** any context error is treated as "the client went away", so when
  `REQUEST_TIMEOUT` expires the stream just stops: no error event, no `[DONE]`.
- **Fix:** when the request context is still alive but the deadline passed,
  send the timeout error event and `[DONE]`. Only stay silent when the client
  disconnected.
- **Tests:** integration test with a one second timeout and `[fake:slow]`,
  streaming, expecting an error event with code `timeout`.

### BUG-2 (Low, S): JSON mode returns empty content when the reply is not JSON

- **Where:** `src/internal/openai/handler.go:412` (non-streaming) compared
  with `:538-541` (streaming sends the raw text).
- **Fix:** in `complete`, when JSON mode is on and no JSON was extracted, use
  `res.Text` as the content. Add a fake directive only if needed to produce a
  non-JSON reply in JSON mode (for example `[fake:not-json]` in `fakecore`).

### BUG-3 (Low, S): A crashed Claude process can look like a complete answer

- **Where:** `src/internal/agent/claude/claude.go:485-491` and the exit status
  ignored in `Run` when a result exists.
- **Fix:** when the stream ends without a `result` line and the process exit
  status is not zero, return an upstream error instead of `finish_reason:
  "stop"`. Add a scripted fake scenario (`truncated`: some text, then exit 3).

### BUG-4 (Low, M): Rate-limited subscriptions are retried on every request

- **Where:** `src/internal/openai/handler.go` (`attempt.run`),
  `src/internal/agent/backend.go` (`Execute`),
  `src/internal/agent/status.go`.
- **Problem:** after a usage-limit error the next request starts a CLI on the
  same subscription again before falling back, adding one to two seconds and a
  process each time.
- **Fix:** record a cooldown per account: until the reset time from Claude's
  rate limit event when one is known, otherwise 60 seconds; 15 seconds for
  "overloaded". In `attempt.run`, move candidates in cooldown to the end
  instead of removing them, so a key with only that subscription still tries.
  Show `cooldown_until` in the admin subscription view and on `/admin`.
- **Tests:** routing test with stub backends and a fake clock or short
  durations.

### BUG-5 (Low, S): Shutdown can leave CLI processes running

- **Where:** `src/internal/server/server.go:205`.
- **Fix:** give the server a cancellable `BaseContext`. If `Shutdown` hits its
  20 second limit, cancel that context (which kills the CLI process groups),
  call `srv.Close()` and wait up to 5 seconds for handlers before returning.

### BUG-6 (Low, S): Configuration accepts values that break the server

- **Where:** `src/internal/config/config.go` (`Load`).
- **Fix:** reject with clear messages: `REQUEST_TIMEOUT` or `QUEUE_TIMEOUT`
  not positive, `MAX_BODY_MB` below 1, `IMAGE_MAX_DIMENSION` or `MAX_IMAGES`
  negative, empty `MODELS` or `CURSOR_MODELS`, and model or alias names that
  fail the SEC-7 pattern. Table-driven tests.

### BUG-7 (Low, S): Key and grant files can drift apart

- **Where:** `src/internal/admin/admin.go` (`createKey`, `deleteKey`).
- **Fix:** if saving grants fails in `createKey`, delete the new key before
  returning the error. In `server.New`, after both stores are open, drop
  grants that name unknown keys and log one line with the count.

### BUG-8 (Low, S): Small correctness issues

1. `src/internal/subscriptions/manager.go:87` writes `acc.Name` on a shared
   account while other goroutines may read it. Remove `Name` from
   `agent.Account` (callers have the subscription) or stop updating it.
2. `moveSubscription` in `src/internal/admin/subscriptions.go` answers 404 for
   every error, including a failed save. Return 500 for anything that is not
   `ErrNotFound`.
3. `src/internal/agent/claude/claude.go:228`: a JSON schema larger than the
   operating system's argument limit fails at process start with a 502. Reject
   schemas over 64 KiB with a 400 in `jsonSchemaFor`. Also cap a single stream
   line at 32 MiB in both parsers (`claude.go:299`, `cursor.go` `parse`).

## Build, CI and Dependencies

### OPS-1 (High, S): Go toolchain has 18 reachable known vulnerabilities

- **Where:** `go.mod:3` (`go 1.26.4`). CI installs that exact version. The
  Dockerfile uses the floating `golang:1.26` tag, so a rebuilt image already
  gets a newer patch release.
- **Advisories reported by `govulncheck`:** GO-2026-5026, 5856, 5972, 6090,
  6218, 6603, 6607, 6608, 6809, 6859 in `net/http`, `crypto/tls`,
  `crypto/x509`, `net/url`, `net/textproto` and `encoding/asn1`. The highest
  fixed version is Go 1.26.11. Details: `https://pkg.go.dev/vuln/<ID>`.
- **Fix:** set the `go` directive to the newest 1.26 patch release (1.26.11 or
  later), run `go mod tidy`, confirm `govulncheck ./...` reports nothing
  reachable, and let CI rebuild the image.

### OPS-2 (Medium, S): CI does not scan for vulnerabilities or lint

- **Fix:** add a `lint` job (or steps in `test`) that runs `staticcheck` and
  `govulncheck` at pinned versions, plus a weekly `schedule` trigger so new
  advisories fail a run even without code changes. Add `make lint` and
  `make vuln` targets and mention them in `docs/DEV.md` and `CLAUDE.md`.

### OPS-3 (Low, S): No automated dependency updates; actions not pinned

- **Fix:** add `.github/dependabot.yml` for `gomod`, `github-actions` and
  `docker` (weekly). Pin the nine actions in `.github/workflows/ci.yml` to
  commit SHAs with the version in a trailing comment.

### OPS-4 (Low, M): Image build is not reproducible or fully verified

- **Where:** `Dockerfile` (base images by tag, `CLAUDE_VERSION=stable`,
  `CURSOR_VERSION=latest`), `scripts/install-cursor.sh` (no checksum).
- **Fix:** pin both base images by digest (Dependabot keeps them fresh). Pin
  explicit Claude and Cursor versions as `ARG` defaults. In
  `install-cursor.sh`, verify the downloaded archive against SHA-256 values
  kept in the repository per version and architecture; fail when the version
  has no recorded checksum unless `CURSOR_ALLOW_UNVERIFIED=1` is set. Record
  the checksums for the pinned version by downloading it once in CI and
  comparing both architectures. Optional: enable `provenance` and `sbom` in
  the build action.

## Maintainability

All QUAL items are behaviour-preserving. Do them after the security and bug
batches so those diffs stay easy to review.

### QUAL-1 (M): `openai/handler.go` does too much

619 lines hold routing, key authentication, the request flow, the fallback
loop, the stop-sequence filter, the SSE writer and error mapping. `complete`
and `stream` take eight and nine positional parameters. Split into `auth.go`,
`attempt.go`, `stopfilter.go`, `stream.go` and `errors.go`, and pass a small
`chatRun` struct (id, created, model, singleTool, includeUsage, loginURL)
instead of the positional parameters.

### QUAL-2 (M): Two very long functions in the backends

`claude.parse` (about 215 lines, six levels of nesting) and `cursor.Run`
(about 135 lines). Turn `parse` into a `streamParser` struct with one method
per event type. Split `cursor.Run` into `prepareWorkspace`, `collectToolCalls`
and `finishResult`. Existing tests must pass unchanged.

### QUAL-3 (S): Error kinds and provider names are bare strings

`"auth"`, `"rate_limit"`, `"overloaded"`, `"upstream"`, `"busy"`,
`"forbidden"`, `"not_found"` and `"invalid_request"` appear as literals in
five files; `"claude"` and `"cursor"` appear as literals in `subscriptions`
and `admin`. Add `agent.Kind...` constants and use `claude.Provider` and
`cursor.Provider` (or move the provider IDs into `agent`).

### QUAL-4 (S): Duplicated helpers in the wrong packages

`writeJSON` exists three times (`admin`, `openai`, `agent/bridge.go`).
`keys.WriteFileAtomic` is used by four packages that have nothing to do with
keys. Create `internal/httpx` (JSON writing) and `internal/fsutil` (atomic
write, and sync the directory after the rename).

### QUAL-5 (M): Admin API responses are untyped maps

`view`, `keyView` and the list handlers build `map[string]any`. Replace them
with response structs with JSON tags so the contract in `docs/API.md` can be
checked by reading one file. The JSON output must be identical; add a golden
test for one subscription and one key.

### QUAL-6 (M): Only one sign-in can run at a time

`agent.LoginManager` (`src/internal/agent/login.go:68`) holds a single
session; starting a sign-in for one subscription cancels another's without
telling anyone. Keep one session per subscription in a map, with a cap of
three running at once. The pages already poll per subscription.

### QUAL-7 (S): No lint configuration; seven unchecked errors

Add `.golangci.yml` enabling `govet`, `staticcheck`, `errcheck`, `gosec`,
`misspell`, `unconvert` and `gocyclo` (threshold 30 to start). Fix the
unchecked errors reported by `gosec` G104 (`server.go:83`, `:93`,
`keys/store.go:253-261`, `agent/login.go:136`, `cmd/creditapi/main.go:74`) by
assigning to `_` with a short reason. Mark the intended subprocess and file
path findings with `//nolint:gosec` and a reason.

### QUAL-8 (S): No panic recovery; server errors bypass JSON logs

Add a recovery middleware that logs `creditapi.http.request.panicked` with the
request ID and returns an OpenAI-style 500 when nothing was written yet. Point
`http.Server.ErrorLog` at the slog logger so TLS and connection errors are
JSON lines too. Add `X-Content-Type-Options: nosniff` to JSON responses.

### Optional (only if time allows)

- Split `src/internal/admin/web/app.js` (711 lines, two pages) into
  `common.js`, `admin.js` and `login.js` as ES modules. The Content Security
  Policy already allows same-origin modules.
- Tag a first release (`v0.1.0`) and keep a short changelog.

## Tests

### TEST-1 (M): Coverage gaps and missing fuzz tests

- `admin` (72.7%): session cookie edge cases (expired, tampered, wrong
  secret), `sameOrigin`, the limiter, error paths of the key handlers.
- `keys` (78.0%): legacy prefix, disabled keys, save failures, concurrent
  `RecordUsage` and `Flush`.
- Commit fuzz tests (they passed 20 second runs during the review) for
  `jpegOrientation`, `stopFilter.push` (invariant: output equals the text up
  to the first stop sequence, for any chunking), `agent.ExtractJSON`,
  `agent.ExtractLoginURL` and the transcript escaping from SEC-12. Keep seed
  corpora small.
- The web pages have no automated test. Optional: one Playwright smoke test
  against `make e2e` that signs in, adds a subscription and ticks an access
  box. Only add it if it stays under a minute in CI.

## Documentation

### DOC-1 (S): Documentation corrections

1. `docs/SETUP.md:241` says start-up takes "a little more for Cursor", and the
   memory figure (250 to 350 MB per process) reads as if it covered both
   providers. Only Claude was measured. Say so, and say Cursor is unmeasured.
2. `docs/SECURITY.md:108` tells people to open a public issue for security
   problems. Point to GitHub's private vulnerability reporting instead.
   **OWNER:** enable it under the repository's Security settings.
3. After the batches land, update `docs/ENVIRONMENT.md` (`IMAGE_MAX_PIXELS`,
   `MAX_PENDING`, `TRUSTED_PROXIES`), `docs/API.md` (health response,
   `invalid_model`, timeout event in streams), `docs/SECURITY.md` (Cursor
   residual risk, session sign-out behaviour) and `CLAUDE.md` (lint targets,
   the note that `.bru` files are edited by hand).

## Suggested Batches

| Batch | Items | Why together |
| --- | --- | --- |
| 1. Urgent | SEC-1, OPS-1, SEC-4, BUG-1 | Small, high value, independent |
| 2. Request limits | SEC-2, SEC-8, SEC-7, BUG-6, BUG-8 | All touch request admission and validation |
| 3. Admin and proxy | SEC-5, SEC-6, SEC-11, SEC-14 | All touch the admin surface |
| 4. Data exposure | SEC-9, SEC-10, SEC-12, SEC-13 | Sanitizing what leaves the server |
| 5. Cursor isolation | SEC-3, SEC-15 | Needs care; list the OWNER steps in the PR |
| 6. Reliability | BUG-2, BUG-3, BUG-4, BUG-5, BUG-7 | Behaviour fixes with new tests |
| 7. Tooling | OPS-2, OPS-3, OPS-4, QUAL-7 | CI and build only |
| 8. Refactors | QUAL-1 to QUAL-6, QUAL-8 | No behaviour change; one PR per item is fine |
| 9. Tests and docs | TEST-1, DOC-1 | Last, so docs match the final behaviour |

## Open Questions for the Owner

1. SEC-3 item 5: does `cursor-agent --sandbox enabled` work in the Docker
   image, and does it confine file reads?
2. SEC-14: which host does the real Cursor sign-in link use?
3. SEC-15: does the container work with a read-only root file system?
4. SEC-6: is any monitor relying on the subscription list in `/healthz`?
5. SEC-11: is "sign out signs every device out" acceptable?
6. Enable GitHub private vulnerability reporting before the repository goes
   public (DOC-1).
