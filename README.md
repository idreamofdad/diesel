# Diesel

[![Claude Logo](https://img.shields.io/badge/Claude-D97757?label=co-authored%20with)](https://claude.ai/code)

Diesel is a desktop app for chatting with an AI companion.

You can type to him, or use your voice he'll listen. He will speak back to you and send you a picture of Diesel. Turn on continuous conversation so you can just talk back and forth.

Everything he needs — the chat model, the voice, the picture generator — runs against services you point him at, so you can mix and match (a cloud provider for the chat, a local model for the voice, your own image server, and so on). Conversations can be saved between sessions so he remembers you next time you open the app.

You don't have to be at the desk, either. The desktop app can expose Diesel over your network so a browser, your phone, or a chat app can join the *same* conversation — a message sent from any client shows up everywhere.

## How it works

Each conversation turn fans out across four independent backends:

- **Chat** — any OpenAI-compatible `/v1/chat/completions` endpoint (cloud APIs, LM Studio, llama.cpp, Ollama).
- **Speech-to-text** — captured from the configured input device gated by a voice-activity detector that auto-stops on trailing silence (with a 30 s hard cap), then uses a Whisper-compatible `/v1/audio/transcriptions` endpoint.
- **Text-to-speech** — the reply text is sent to an OpenAI-compatible `/v1/audio/speech` endpoint and streamed to the output device. Starting a new recording interrupts in-flight playback so the VAD doesn't pick up Diesel talking over the user.
- **Image generation** — a ComfyUI server driven over its WebSocket API using a bundled workflow (`default_workflow.json`). The reply's emotion and nudity flag are spliced into the prompt; preview frames stream into the portrait pane during diffusion, with the final PNG cached for the full-size viewer.

Settings and conversation history are persisted as JSON under the OS user config directory.

## The shared hub

All the pipelines above are owned by a single in-process *hub* that is the one source of truth for the conversation. The desktop GUI is just one subscriber; every other client below subscribes to the same hub. Whoever sends a message — desktop, browser, phone, SMS, Telegram — appends to one shared history and triggers one set of pipelines, and the result is broadcast to everyone. Turns are processed one at a time; a message sent while a turn is in flight is rejected as busy (or queued, for Telegram).

## Connecting other clients

Enable the server from Settings → Server (and "Expose on network" to bind `0.0.0.0` so other devices on the LAN can reach it). An optional auth token gates every client. From there:

- **Browser remote UI** — open the server's address in a browser for a full chat UI: typing, voice with a recording mic button, the streaming portrait, and a settings panel backed by the REST API. It's a Svelte app served by the desktop binary.
- **SMS** — Diesel polls a Twilio number for inbound texts and replies by SMS through the same hub. Configure the Twilio credentials and number in Settings.
- **Telegram** — point a Telegram bot token at Diesel and the bot bridges chats into the hub over the Bot API's long-poll, so it works behind NAT with no public webhook.
- **`/api/v1` HTTP + WebSocket API** — a versioned surface for building native clients (e.g. an Android app). REST endpoints cover state, sending, voice upload, and media fetch; a WebSocket streams turn, audio, and portrait events. The full contract is in [`docs/api-v1.md`](docs/api-v1.md), with a machine-readable OpenAPI 3.1 spec served at `/openapi.json`.

## Running headless

Diesel also builds as a headless daemon, **`dieseld`** — the hub, the HTTP server (web UI + `/api/v1`), and the SMS/Telegram/Matrix bridges, with no native window or audio. Voice still works end to end: the browser does capture, voice-activity detection, speech-to-text upload, and playback. The daemon contains **no cgo**, so it builds as a single static binary that cross-compiles cleanly and runs anywhere:

```
make daemon                                  # -> bin/dieseld
# or directly:
CGO_ENABLED=0 go build -tags goolm ./cmd/dieseld
```

Run it pointed at a data directory; the browser is the UI, and the web settings panel covers the chat/voice/image config:

```
dieseld -port 8080 -listen-all -auth-token <secret>
```

Bridge credentials are host-bound and read from the same `diesel.db` the desktop app writes, so configure those once with the desktop app (or share the database). The desktop app (`make desktop`) keeps the native window and native audio and requires cgo.

## License

Released under the MIT License. See [LICENSE](LICENSE) for the full text.