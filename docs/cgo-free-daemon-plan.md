# Plan: optional cgo-free `dieseld` server target

Goal: produce a fully static, **cgo-free** build of Diesel for headless
boxes (servers, containers, SBCs) **without giving up the native desktop
app**. One codebase, two targets:

- `cmd/diesel` — the existing Fyne desktop app. Native window, native
  audio. Requires cgo. **Unchanged in behavior.**
- `cmd/dieseld` — new headless daemon. Hub + HTTP server (web UI) +
  bridges. No window, no native audio: the browser does capture/VAD/STT/
  TTS. Builds with `CGO_ENABLED=0`.

The desktop app is never removed. Everything here is additive plus an
internal reorganization behind build tags.

## Why this is the only cgo-free shape

A native desktop window needs OpenGL via cgo (Fyne), and microphone
capture needs miniaudio via cgo (malgo). Neither has a mature pure-Go
replacement, so a cgo-free build cannot have a native window — its UI is
the browser. The web frontend already drives the full voice loop
(`@ricky0123/vad` in `MicButton.svelte` → `/api/v1/transcribe` →
`/api/v1/audio/:id`), so headless loses no functionality, only the
window.

What pulls in cgo today, and its fate in the daemon build:

| Source | Fate under `CGO_ENABLED=0 -tags goolm` |
|---|---|
| Fyne (glfw/gl/systray/go-locale) | excluded — `cmd/dieseld` has no Fyne import |
| malgo (audio device I/O) | excluded — gated behind `//go:build cgo` |
| `mattn/go-sqlite3` (via mautrix `dbutil/litestream`) | gone — litestream's `nocgo.go` path; storage already uses pure-Go `modernc` |
| libolm (Matrix E2EE) | replaced by pure-Go goolm via `-tags goolm` |

## Decisions (resolved)

1. **Split malgo via in-place build tags.** Device I/O moves into
   `*_cgo.go` files (`//go:build cgo`) inside `internal/audio` and
   `internal/tts`; the pure-Go HTTP/codec code stays untagged. No stub
   functions — nothing in the server path references device symbols.
2. **`cmd/dieseld` + shared `internal/app` bootstrap.** The identical
   store + settings backend + hub + bridge-manager wiring is extracted so
   the two entrypoints can't drift.
3. **Always-on server, configured by flags.** The daemon HTTP server is
   the whole point, so it always listens — `-port`, `-listen-all`,
   `-auth-token`, reusing `server.Manager.Apply` via a synthesized
   settings snapshot (no changes to `server.Manager`).
4. **All four bridges included** (SMS, Telegram, Matrix). Matrix is
   cgo-free with goolm.
5. **Bridges configured out-of-band.** The web UI deliberately can't
   retune host-bound credentials (`mergeFromWeb`), so the daemon applies
   whatever is already in `diesel.db` at startup; runtime reconfiguration
   of bridges over the web is out of scope for v1.
6. **No changes to `server.Manager`.**
7. **Native desktop app preserved; daemon is an optional extra.**

## Work breakdown

1. **Split `internal/audio`** — `device_cgo.go` (`//go:build cgo`) gets
   `Recorder`, `StartRecording`, `feed`/`Stop`/`finish`, `frameRMS`, the
   VAD constants, `Context`/`sharedCtx`, device enumeration, `pickDevice`,
   `PickOutputDevice`, `StopReason`, and the malgo import. `audio.go`
   keeps `Transcribe`/`TranscribeBlob`/`EncodeWAV` + the STT format
   constants. Device/VAD tests move to a `//go:build cgo` test file.
2. **Split `internal/tts`** — `play_cgo.go` (`//go:build cgo`) gets
   `Speaker`, `Play`, `read`, `drainThenFinish`, `finish`, the buffering
   constants, `sampleFormatFor`, and the malgo import. `tts.go` keeps
   `Synthesize`. `TestSampleFormatFor` moves to a `//go:build cgo` test.
3. **Tag cgo-only commands** — add `//go:build cgo` to all
   `cmd/diesel/*.go` and `cmd/voicecheck/main.go` so
   `CGO_ENABLED=0 go build/vet/test ./...` skips them cleanly.
4. **`internal/app` bootstrap** — `Wire(ctx) (*Deps, func())` returning
   store, hub, the three bridge managers (applied), and an un-applied
   `*server.Manager`; the returned func tears everything down in order.
   Refactor `cmd/diesel/main.go` onto it.
5. **`cmd/dieseld/main.go`** — flags (`-data-dir`, `-port`, `-listen-all`,
   `-auth-token`); `app.Wire`; synthesize an always-on server settings
   snapshot → `srvMgr.Apply`; serve `dieselweb.DistFS()`; block on
   `signal.NotifyContext` (SIGINT/SIGTERM) → teardown. No hub subscriber.
6. **Makefile + docs + validation** — `make desktop` / `make daemon`;
   confirm `CGO_ENABLED=0 go build -tags goolm ./cmd/dieseld` and that its
   dependency graph contains zero cgo packages.

## Build commands

```
# Desktop (native window + audio) — requires cgo
CGO_ENABLED=1 go build ./cmd/diesel            # uses libolm; or add -tags goolm

# Daemon (headless, static, cgo-free)
CGO_ENABLED=0 go build -tags goolm ./cmd/dieseld
```

## Out of scope (v1)

- Runtime reconfiguration of bridge credentials over the web.
- A `dieseld config` seeding tool for `diesel.db`.
- Arbitrary `-addr host:port` binding (only loopback / all-interfaces).
