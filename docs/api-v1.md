# Diesel API v1

The `/api/v1` namespace exposes the Diesel chat over HTTP + WebSocket so
external clients (e.g. an Android app) can drive the same conversation as
the desktop GUI and web UI.

All clients — desktop, web, SMS, Telegram, and any API client — share **one
conversation**. A message sent from the Android app appears in the desktop
transcript and vice versa.

## Base URL

`http://<host>:<port>/api/v1` — the host/port are set in Diesel's Settings
(Server tab). Enable "Expose on network" to bind `0.0.0.0` so a phone on the
same LAN can reach it. The transport is plain HTTP; an Android app must
allow cleartext to the LAN host via a network-security-config.

## Authentication

If a server auth token is configured, every request must carry it:

- HTTP: `Authorization: Bearer <token>`
- WebSocket: `?token=<token>` query parameter (the bearer header also works
  for native clients that can set headers on the upgrade request)

A blank token disables auth. A wrong/missing token returns `401`.

## REST endpoints

### `GET /state`

Snapshot for a freshly-connected client.

```json
{
  "history": [ { "role": "user", "content": "hi", "timestamp": "..." } ],
  "in_flight": false,
  "status": "Ready",
  "portrait_url": "/api/v1/portrait/<id>"
}
```

`portrait_url` is present only when a portrait has been rendered. `role` is
one of `user`, `assistant`, `system`; assistant messages may carry an
`emotion` field.

### `POST /send`

Post a user message. Body:

```json
{ "text": "hello", "origin": "<client_id>" }
```

`origin` should be the client's stable ID so reply audio routes back to it.
Responses: `202` `{"ok":true}` on accept, `400` on empty text, `409`
`{"error":"busy"}` when another turn is in flight.

### `POST /clear`

Wipes the conversation. `204` on success, `409` while a turn is in flight.

### `POST /transcribe`

Multipart upload for voice input. Form fields: `file` (audio blob) and
`origin` (client ID). The server runs STT, then feeds the recognized text
into the conversation as a turn. Returns `{"text":"...","sent":true}`.

### `GET /portrait/:id`, `GET /portrait-preview/:id`, `GET /audio/:id`

Fetch media by the ID embedded in WebSocket event URLs. `:id` of `latest`
on `/portrait` returns the freshest portrait. Media evicts from a small
cache within seconds — fetch promptly after the event arrives.

### `GET /settings`, `POST /settings`, `POST /settings/models`, `POST /settings/test`, `POST /settings/test-tts`

Read/write the app configuration. Not needed for a chat-only client;
secrets are returned masked as `********`.

## WebSocket — `GET /ws`

`ws://<host>:<port>/api/v1/ws?client_id=<id>&token=<token>`

The client picks its own stable `client_id` (a persisted UUID) and reuses
it across reconnects so reply-audio routing stays consistent. The hub never
closes the socket itself — only network errors or the server stopping do.
The server pings every 25s; reconnect with backoff on drop and re-fetch
`GET /state` on resume.

### Client → server commands

```json
{ "type": "send", "text": "hello" }
{ "type": "clear" }
{ "type": "ping" }
```

`send` over the socket uses the connection's `client_id` as the origin
automatically. Unknown commands are ignored.

### Server → client events

Every frame is a JSON object with a `type` field:

| type                | meaning |
|---------------------|---------|
| `ack`               | sent right after upgrade — seeds `client_id`, `status`, `in_flight`, `portrait_url` |
| `turn_started`      | a turn began; carries the `user` message |
| `turn_complete`     | assistant reply ready; carries `assistant`, `emotion`, `naked`, `usage` |
| `audio_ready`       | TTS done; `audio_url` set if synthesis produced audio |
| `portrait_ready`    | portrait done; `portrait_url` set if image gen produced one |
| `portrait_progress` | intermediate render frame; `portrait_url` points at a preview, `step`/`total` give progress |
| `turn_error`        | the turn failed; `error` holds the message |
| `status`            | free-form status-bar string in `status` |
| `cleared`           | the conversation was wiped |
| `busy`              | a `send` was rejected because a turn is in flight |

Common fields: `origin` (the client_id that started the turn), `turn_id`
(monotonic per-turn counter, correlates later media events to their turn),
`timestamp`.

**Audio routing:** `audio_ready` is broadcast to everyone, but only the
client whose `client_id` equals the event's `origin` should fetch and play
the audio. An empty `audio_url` means "no audio for this turn" (TTS off or
synthesis failed) — not an error.

Portrait events are broadcast to every client regardless of origin.
