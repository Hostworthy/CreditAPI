#!/usr/bin/env bash
# Headless end-to-end run: the real server with the fake Claude and Cursor CLIs, exercised
# by the Bruno collection. Used by `make e2e-ci` and CI.
#
#   BRU      command that runs the Bruno CLI (default: npx @usebruno/cli@4)
#   ENV_FILE env file for the server (default: .env.bruno, else the example)
set -euo pipefail
cd "$(dirname "$0")/.."

BRU="${BRU:-npx --yes @usebruno/cli@4}"
ENV_FILE="${ENV_FILE:-.env.bruno}"
[ -f "$ENV_FILE" ] || ENV_FILE=.env.bruno.example
PORT="$(sed -n 's/^PORT=//p' "$ENV_FILE" | tail -n 1)"
PORT="${PORT:-18180}"

go build -o bin/creditapi ./src/cmd/creditapi
go build -o bin/fakeclaude ./src/cmd/fakeclaude
go build -o bin/fakecursor ./src/cmd/fakecursor

DATA_DIR="$(mktemp -d)"
LOG="$DATA_DIR/server.log"
ENV_CREDITAPI="$PWD/$ENV_FILE" DATA_DIR="$DATA_DIR/data" PORT="$PORT" \
  ./bin/creditapi >"$LOG" 2>&1 &
SERVER_PID=$!

cleanup() {
  status=$?
  kill "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  if [ "$status" -ne 0 ]; then
    echo "--- server log (last 40 lines) ---" >&2
    tail -n 40 "$LOG" >&2 || true
  fi
  rm -rf -- "$DATA_DIR"
  exit "$status"
}
trap cleanup EXIT

for _ in $(seq 1 50); do
  if PORT="$PORT" ./bin/creditapi healthcheck 2>/dev/null; then
    break
  fi
  sleep 0.2
done
PORT="$PORT" ./bin/creditapi healthcheck

mkdir -p bruno/results
cd bruno
$BRU run --env local --env-var "baseUrl=http://127.0.0.1:$PORT" \
  --reporter-junit results/junit.xml --reporter-html results/report.html
