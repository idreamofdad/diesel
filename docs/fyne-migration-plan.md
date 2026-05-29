# Migration Plan: Qt (miqt) → Fyne

Goal: replace the Qt GUI with a Fyne GUI while keeping STT (with VAD) and TTS
fully working. Target branch: `fyne`.

## Core principle

The speech *logic* is already GUI-agnostic. The only Qt coupling is in five
files; everything else (`hub`, `storage`, `server`, `settings`, all four
bridges, `wav`, `comfyui`, `tracing`) has zero Qt and is untouched.

| File | Qt usage | Disposition |
|---|---|---|
| `cmd/diesel/main.go` | Entire UI (~1,366 lines) | Rewrite in Fyne |
| `internal/audio/audio.go` | `QAudioSource` capture + `QMediaDevices` enum | Swap I/O layer; VAD/STT untouched |
| `internal/tts/tts.go` | `QAudioSink` playback | Swap playback layer; `Synthesize` untouched |
| `internal/util/util.go` | `QTimer` in `PollAsync` | Rewrite helper (goroutine + `fyne.Do`) |
| `internal/chat/chat.go` | `QTextEdit` in `AppendTurn` | Retarget to Fyne transcript |

The hub already speaks Go channels (`desktopSub.Events`), so the UI boundary is
clean — only the *drain mechanism* (a `QTimer`) changes.

## Key dependency decisions

- **Audio I/O:** [malgo](https://github.com/gen2brain/malgo) (miniaudio
  bindings) for both capture and playback. One dependency covers capture,
  playback, and device enumeration, and supports per-stream sample rates
  (16 kHz capture vs ~24 kHz TTS WAVs). The VAD already consumes raw int16 PCM,
  so malgo's data callback drops into the existing `consume()` logic.
- **CGo stays enabled** (Fyne's GL driver needs it), but the
  `CGO_CXXFLAGS="-std=c++17"` requirement goes away once miqt is gone — malgo is
  C, not C++.

## Phase 1 — Audio I/O swap (do first, validate in isolation)

The keystone and the only real-risk part. Prove it before touching the GUI.

**`internal/audio/audio.go`:**
- Keep `Recorder`, `StopReason`, the VAD constants, `frameRMS`, `consume`,
  `EncodeWAV`, `Transcribe`/`TranscribeBlob` exactly as-is.
- Replace `StartRecording`'s `QAudioSource`/`OnReadyRead` with a malgo capture
  device (16 kHz, mono, int16). The malgo data callback feeds bytes into the
  same `consume()` VAD state machine. `Stop()` closes the malgo device.
- The malgo data callback runs on an audio thread (Qt's `OnReadyRead` ran on the
  main thread). The VAD math is fine off-thread, but the UI-side `onStop`
  closure in `main.go` must wrap widget work in `fyne.Do(...)`.
- Replace `InputDescriptions`/`OutputDescriptions`/`PickInputDevice`/
  `PickOutputDevice` with `malgo.Context.Devices(...)`, matching saved devices
  by name as today.

**`internal/tts/tts.go`:**
- Keep `Synthesize`, `Speaker`, `OnDone`/`naturalEnd`/`Stop` semantics, and
  `wav.Parse` decoding untouched.
- Replace `Play`'s `QAudioSink`/`QBuffer` with a malgo playback device whose
  data callback pulls from the decoded PCM. Buffer drained → set `naturalEnd`,
  run `cleanup()` → fire `OnDone` (continuous-conversation re-arm unchanged).

**Validation:** a throwaway `main` (or manual test) that records → VAD-stops →
`Transcribe` → `Synthesize` → `Play`, with no GUI.

## Phase 2 — Concurrency helpers

- Rewrite `util.PollAsync` to `go func(){ r := work(); fyne.Do(func(){
  onDone(r) }) }()` — drops the `QTimer`, keeps the generic signature so call
  sites are unchanged.
- Replace the `QTimer` drain pump (`main.go:551-567`) with a goroutine ranging
  over `desktopSub.Events`, dispatching each via `fyne.Do`.

## Phase 3 — GUI rebuild (`cmd/diesel/main.go` + `chat.AppendTurn`)

Widget mapping:

| Qt | Fyne |
|---|---|
| `QMainWindow` | `app.New()` + `Window` |
| `QTextEdit` transcript | `widget.RichText` (colored segments) in `container.NewVScroll` |
| `QLineEdit` input | `widget.Entry` w/ `OnSubmitted` |
| `QPushButton` | `widget.Button` |
| portrait `QLabel`+pixmap | `canvas.Image` from PNG bytes |
| status bar | `widget.Label` |
| `QTabWidget` settings | `dialog.NewCustom` + `container.AppTabs` |
| `QComboBox` (read-only) | `widget.Select` |
| `QComboBox` (editable: model/STT/TTS) | `widget.SelectEntry` |
| `QSpinBox` | numeric `widget.Entry` + validator (no stable Fyne spinbox) |
| `QCheckBox` | `widget.Check` |
| password fields | `widget.Entry{Password: true}` |
| `QMessageBox` | `dialog.ShowConfirm`/`ShowError` |
| menu (New / Settings) | `fyne.MainMenu` |
| full-size portrait popup | secondary `Window` with `canvas.Image` |
| dark stylesheet | custom `fyne.Theme` |

- `chat.AppendTurn(...)` appends a colored `RichText` segment instead of HTML
  into `QTextEdit`.
- Drop `runtime.LockOSThread` in `init` — Fyne's driver manages the main thread.

Cosmetic caveats: `SelectEntry` and the validated-entry spinbox are clunkier
than Qt's equivalents, and `RichText` is less rich than `QTextEdit`'s HTML — the
transcript will look plainer. Functionally equivalent.

## Phase 4 — Cleanup

- Remove `github.com/mappu/miqt` from `go.mod`; `go mod tidy`.
- Drop the `CGO_CXXFLAGS="-std=c++17"` build prefix; update build docs/memory.
- Update README / run instructions.

## Effort & risk

- Largest chunk: GUI rebuild (Phase 3) — mechanical, ~1,400 lines.
- Highest risk: audio I/O (Phase 1) — small, isolated, independently testable.
- Free: STT, VAD, TTS synthesis, hub, storage, server, bridges.