<!-- markdownlint-disable MD033 MD041 -->

<div align="center">

# CreditAPI

<img src="docs/images/readme-hero.svg" alt="An app talks to your server through the OpenAI API, and your server answers with the Claude Code or Cursor CLI" width="420" />

[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![OpenAI API](https://img.shields.io/badge/API-OpenAI%20compatible-412991)](docs/API.md)
[![Claude Code](https://img.shields.io/badge/Claude%20Code-CLI-D97757)](https://code.claude.com/docs)
[![Cursor](https://img.shields.io/badge/Cursor-CLI-000000)](https://cursor.com/docs/cli/overview)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](docs/SETUP.md)
[![Bruno](https://img.shields.io/badge/Bruno-e2e%20tests-F4AA41)](bruno)
[![License: PolyForm Noncommercial](https://img.shields.io/badge/License-PolyForm%20Noncommercial-blue.svg)](LICENSE)

</div>

<!-- markdownlint-enable MD033 MD041 -->

CreditAPI turns your Claude or Cursor subscriptions into a private,
OpenAI- and Anthropic-compatible API for your own apps. Anything that can talk
to an "OpenAI compatible" or Anthropic provider can use them through it: chat assistants, self-hosted
tools, scripts and home lab services, with tool calls, photos and structured
answers.

It runs as one Docker container next to your apps. Every request is answered
by the official Claude Code or Cursor CLI signed in with your account, so there
is no separate API bill. You connect each subscription once from your phone,
give each app its own key, and decide which subscriptions each key may use.
When one subscription is signed out or at its limit, the next one answers, and
the server tells you when a login needs renewing.

## Where to Go

| Page       | What it is for                                                        |
| ---------- | --------------------------------------------------------------------- |
| `/v1`      | The API your apps call: OpenAI-compatible (`/v1/chat/completions`) and Anthropic-compatible (`/v1/messages`) |
| `/login`   | Connect or renew a subscription (works well on a phone)                |
| `/admin`   | Add subscriptions, create API keys and choose which keys use which     |
| `/healthz` | Health and login status for uptime monitors                           |

Open `/login` or `/admin` in a browser on your server's address and sign in
with the admin password you configured.

## Get Started

See [SETUP.md](./docs/SETUP.md) to run it with Docker, connect your
subscriptions and point your apps at it.

## For Developers

Working on the codebase?

- See [DEV.md](./docs/DEV.md) for setup, commands, tests and architecture.
- See [API.md](./docs/API.md), [ENVIRONMENT.md](./docs/ENVIRONMENT.md) and [SECURITY.md](./docs/SECURITY.md) for reference.
- See [CLAUDE.md](./CLAUDE.md) for AI assistants.

## Good to Know

This project is meant for personal use with subscriptions you are allowed to
use this way. Check Anthropic's
[consumer terms](https://www.anthropic.com/legal/consumer-terms) and Cursor's
[terms of service](https://cursor.com/terms-of-service) for your plans, do not
share access with other people, and only connect a work or someone else's
subscription if its owner and their policies allow it. Answers come from an AI
model and can be wrong, so double-check anything that matters. Licensed under the
[PolyForm Noncommercial License 1.0.0](LICENSE): free for personal and other
noncommercial use; commercial use needs a separate license from the copyright
holder. Copies taken before this change keep their MIT license.
