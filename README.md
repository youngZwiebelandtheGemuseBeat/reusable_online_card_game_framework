# reusable_online_card_game_framework
reusable card game framework 2025

# Quick start

## 1) Server
export PORT=8080
export ORIGIN_ALLOWLIST=http://localhost:8080,http://localhost:5173
cd server && go run ./cmd/server

## 2) Client (Web)
# ensure Flutter SDK is installed
cd client
flutter pub get
# run in Chrome
flutter run -d chrome --dart-define=WS_URL=ws://localhost:8080/ws

## 3) Test
- You should see a connection and a `joined` message.
- Click the "Chat: hi" button → all connected clients receive it.

## 4) Next steps
- Wire rooms and the Lua engine into the ws hub.
- Flesh out Mulatschak flow: deal → (auto‑mulatschak check / bidding) → play → score.
- Add Supabase Auth + Postgres, Redis queues, matchmaking, reconnect+bot.
- Deploy server on Render (free), web on Cloudflare Pages.