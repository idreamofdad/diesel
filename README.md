# Diesel

[![Claude Logo](https://img.shields.io/badge/Claude-D97757?label=co-authored%20with)](https://claude.ai/code)

Diesel is a desktop app for chatting with an AI companion.

You can type to him, or use your voice he'll listen. He will speak back to you and send you a picture of Diesel. Turn on continuous conversation so you can just talk back and forth.

Everything he needs — the chat model, the voice, the picture generator — runs against services you point him at, so you can mix and match (a cloud provider for the chat, a local model for the voice, your own image server, and so on). Conversations can be saved between sessions so he remembers you next time you open the app.

## How it works

Each conversation turn fans out across four independent backends:

- **Chat** — any OpenAI-compatible `/v1/chat/completions` endpoint (cloud APIs, LM Studio, llama.cpp, Ollama).
- **Speech-to-text** — captured from the configured input device gated by a voice-activity detector that auto-stops on trailing silence (with a 30 s hard cap), then uses a Whisper-compatible `/v1/audio/transcriptions` endpoint.
- **Text-to-speech** — the reply text is sent to an OpenAI-compatible `/v1/audio/speech` endpoint and streamed to the output device. Starting a new recording interrupts in-flight playback so the VAD doesn't pick up Diesel talking over the user.
- **Image generation** — a ComfyUI server driven over its WebSocket API using a bundled workflow (`default_workflow.json`). The reply's emotion and nudity flag are spliced into the prompt; preview frames stream into the portrait pane during diffusion, with the final PNG cached for the full-size viewer.

Settings and conversation history are persisted as JSON under the OS user config directory.