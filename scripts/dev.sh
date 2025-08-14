#!/usr/bin/env bash
set -euo pipefail

# Kill background server on exit
cleanup() { [[ -n "${SERVER_PID:-}" ]] && kill "${SERVER_PID}" || true; }
trap cleanup EXIT

# 1) Server
(
  cd server
  export PORT=8080
  export ORIGIN_ALLOWLIST="http://localhost:5173,http://127.0.0.1:5173"
  go run ./cmd/server
) &
SERVER_PID=$!

# 2) Client (foreground)
cd client
flutter pub get
flutter run -d web-server --web-port 5173 --dart-define=WS_URL=ws://127.0.0.1:8080/ws
