package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"diesel/internal/audio"
	"diesel/internal/chat"
	"diesel/internal/hub"
	"diesel/internal/matrix"
	"diesel/internal/server"
	"diesel/internal/settings"
	"diesel/internal/sms"
	"diesel/internal/storage"
	"diesel/internal/telegram"
	"diesel/internal/tracing"
	"diesel/internal/tts"
	"diesel/internal/util"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// Transcript speaker-label colors, expressed as theme color names because
// RichText can only color text by name. Primary (blue) labels Diesel; Error
// (red) labels the user and inline error rows — matching the Qt palette.
const (
	labelDiesel = theme.ColorNamePrimary
	labelUser   = theme.ColorNameError
)

// desktopOrigin is the subscriber ID the desktop UI registers with the hub.
// Stable string (not a UUID) so the hub's "last-active wins" TTS routing and
// the busy-broadcast filter both know who "the desktop" is.
const desktopOrigin = "desktop"

// uiAsync runs work off the UI goroutine and delivers its result to onDone
// back on the UI goroutine via fyne.Do. It's the GUI-side wrapper around
// util.Async — util stays toolkit-agnostic (see internal/util), and the
// fyne.Do marshalling lives here where Fyne is in scope.
func uiAsync[T any](work func() T, onDone func(T)) {
	util.Async(work, func(r T) { fyne.Do(func() { onDone(r) }) })
}

func main() {
	// -data-dir overrides where the database and character image live; blank
	// keeps the platform user-config default. Parsed first so it's in effect
	// before anything resolves a config path.
	dataDir := flag.String("data-dir", "", "directory for Diesel's data (database, character image); defaults to the OS user config dir")
	flag.Parse()
	if *dataDir != "" {
		util.SetConfigDir(*dataDir)
	}

	// OpenTelemetry: a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT (or the
	// trace-specific override) is set. Shutdown flushes in-flight spans on
	// exit, bounded to 5 s so a stuck collector can't hang the app.
	if shutdown, err := tracing.Init(context.Background()); err != nil {
		log.Printf("[tracing] init failed: %v", err)
	} else if shutdown != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				log.Printf("[tracing] shutdown: %v", err)
			}
		}()
	}

	// SQLite-backed persistence: conversation history, the settings blob, and
	// each bridge's bookkeeping all live in one database file. Opened before
	// anything reads settings or history.
	dbPath, err := util.ConfigFilePath("diesel.db")
	if err != nil {
		log.Fatalf("[storage] config path: %v", err)
	}
	store, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("[storage] open: %v", err)
	}
	defer func() { _ = store.Close() }()
	// Wire the settings package to the database. settings can't import storage
	// (storage imports settings), so persistence is injected.
	settings.SetBackend(
		func() settings.AppSettings {
			s, err := store.LoadSettings(context.Background())
			if err != nil {
				log.Printf("[settings] load: %v", err)
			}
			return s
		},
		func(s settings.AppSettings) error {
			return store.SaveSettings(context.Background(), s)
		},
	)

	// Hub owns the conversation. Started before any UI so the persisted
	// history is loaded by the time the window paints it.
	h := hub.New(store)
	h.Start(context.Background())

	// HTTP server + bridges. Each has the same Apply/Stop shape — opt-in via
	// settings, hot-reapplied on every Save.
	srvMgr := server.New(h, embeddedWebFS())
	srvMgr.Apply(settings.Load())
	defer srvMgr.Stop()
	smsMgr := sms.New(h, store)
	smsMgr.Apply(settings.Load())
	defer smsMgr.Stop()
	tgMgr := telegram.New(h, store)
	tgMgr.Apply(settings.Load())
	defer tgMgr.Stop()
	mxMgr := matrix.New(h, store)
	mxMgr.Apply(settings.Load())
	defer mxMgr.Stop()

	// ─── Fyne app + window ─────────────────────────────────────────────────
	a := app.New()
	a.Settings().SetTheme(newDieselTheme())
	win := a.NewWindow("Diesel")
	win.Resize(fyne.NewSize(960, 560))

	transcript := newTranscriptView()
	portrait := newPortraitPane(win)
	// Seed the portrait from the hub's cached image, if any.
	if _, png := h.LatestPortrait(); len(png) > 0 {
		portrait.show(png, true)
	}
	// Paint any previously-persisted history the hub loaded on Start.
	for _, m := range h.History() {
		switch m.Role {
		case chat.RoleUser:
			transcript.appendTurn("You", m.Content, labelUser)
		case chat.RoleAssistant:
			transcript.appendTurn("Diesel", m.Content, labelDiesel)
		}
	}

	// Status bar.
	statusLabel := widget.NewLabel("Ready")
	statusLabel.Truncation = fyne.TextTruncateEllipsis
	setStatus := func(msg string) { statusLabel.SetText(msg) }
	tokensLabel := widget.NewLabel("")
	footer := container.NewVBox(
		widget.NewSeparator(),
		container.NewBorder(nil, nil, nil, tokensLabel, statusLabel),
	)

	// ─── Shared UI state ───────────────────────────────────────────────────
	// Touched only on the Fyne UI goroutine: widget handlers run there, and
	// every off-thread callback (hub drain, recording onStop, TTS OnDone,
	// uiAsync) marshals back via fyne.Do before reading or writing these.
	var (
		lastDesktopWasVoice bool
		voice               *tts.Speaker
		rec                 *audio.Recorder
		recording           bool
		inFlight            bool
	)

	// Mutually recursive closures (voice → reply → listen again). Declared up
	// front, assigned below.
	var (
		startListening     func()
		sendDesktopMessage func(text string, viaVoice bool)
	)

	message := widget.NewEntry()
	message.SetPlaceHolder("Type a message...")
	sendBtn := widget.NewButton("Send", nil)

	const (
		recordGlyphIdle   = "●"
		recordGlyphActive = "■"
	)
	recordBtn := widget.NewButton(recordGlyphIdle, nil)
	commitBtn := widget.NewButton("↑", nil)

	// Three orthogonal axes gate the text input + Send button: recording (voice
	// capture in progress), inFlight (the hub is mid-turn, any origin), and
	// identity (first/last/pet name all filled). refreshInputEnabled collapses
	// them into one place so every site that flips an axis stays consistent.
	const identityHint = "Set first name, last name, and pet name in Settings → LLM."
	refreshInputEnabled := func() {
		identity := settings.Load().IdentityConfigured()
		if !recording && !inFlight && identity {
			message.Enable()
			sendBtn.Enable()
		} else {
			message.Disable()
			sendBtn.Disable()
		}
		if identity {
			message.SetPlaceHolder("Type a message...")
		} else {
			message.SetPlaceHolder(identityHint)
		}
	}
	setRecordingUI := func(active bool) {
		recording = active
		if active {
			recordBtn.SetText(recordGlyphActive)
		} else {
			recordBtn.SetText(recordGlyphIdle)
		}
		refreshInputEnabled()
	}

	sendDesktopMessage = func(text string, viaVoice bool) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		message.SetText("")
		lastDesktopWasVoice = viaVoice
		if err := h.Send(context.Background(), text, desktopOrigin, false); err != nil {
			setStatus("✗ " + err.Error())
		}
	}

	// sttResult carries the transcription outcome from the worker goroutine.
	type sttResult struct {
		text string
		err  error
	}
	startListening = func() {
		if rec != nil {
			return
		}
		if voice != nil {
			voice.Stop()
			voice = nil
		}
		r, err := audio.StartRecording(context.Background(), func(pcm []byte, reason audio.StopReason) {
			// onStop fires on a non-UI goroutine — marshal everything onto
			// the UI thread before touching widgets or shared state.
			fyne.Do(func() {
				rec = nil
				setRecordingUI(false)
				switch reason {
				case audio.StopCancelled:
					setStatus("Ready")
					return
				case audio.StopNoSpeech:
					setStatus("No speech detected")
					return
				}
				setStatus("Transcribing…")
				s := settings.Load()
				ep := util.FirstNonEmpty(s.STTEndpoint, s.APIEndpoint)
				key := util.FirstNonEmpty(s.STTAPIKey, s.APIKey)
				wavBytes := audio.EncodeWAV(pcm)
				uiAsync(
					func() sttResult {
						t, err := audio.Transcribe(context.Background(), ep, key, s.STTModel, wavBytes)
						return sttResult{t, err}
					},
					func(res sttResult) {
						if res.err != nil {
							setStatus("✗ " + res.err.Error())
							return
						}
						if strings.TrimSpace(res.text) == "" {
							setStatus("No speech detected")
							return
						}
						message.SetText(res.text)
						sendDesktopMessage(res.text, true)
					},
				)
			})
		})
		if err != nil {
			setStatus("✗ " + err.Error())
			return
		}
		rec = r
		setStatus("Recording…")
		setRecordingUI(true)
	}

	sendBtn.OnTapped = func() { sendDesktopMessage(message.Text, false) }
	message.OnSubmitted = func(s string) { sendDesktopMessage(s, false) }
	recordBtn.OnTapped = func() {
		if rec != nil {
			rec.Stop(audio.StopCancelled)
			return
		}
		startListening()
	}
	commitBtn.OnTapped = func() {
		if rec != nil {
			rec.Stop(audio.StopCommitted)
		}
	}

	// ─── Hub subscription + dispatch ───────────────────────────────────────
	// The desktop UI is one of N possible clients. The hub is the canonical
	// writer; this subscription is the only path that updates the transcript,
	// portrait, status bar, and token counter. dispatchEvent always runs on
	// the UI goroutine (the drain loop below marshals via fyne.Do).
	desktopSub := h.Subscribe(desktopOrigin)

	dispatchEvent := func(ev hub.Event) {
		switch ev.Type {
		case hub.EventTurnStarted:
			if ev.User != nil {
				transcript.appendTurn("You", ev.User.Content, labelUser)
			}
			// Lock the UI regardless of origin — only one turn runs at a time,
			// so remote-initiated turns disable Send too.
			inFlight = true
			refreshInputEnabled()
		case hub.EventTurnComplete:
			// Text-only event — audio and portrait arrive on their own events.
			// Re-enable input the moment text is here so the next turn can
			// start while media is still rendering.
			if ev.Assistant != nil {
				transcript.appendTurn("Diesel", ev.Assistant.Content, labelDiesel)
			}
			inFlight = false
			refreshInputEnabled()
			win.Canvas().Focus(message)
			if ev.Usage != nil {
				total := ev.Usage.TotalTokens
				if total == 0 {
					total = ev.Usage.PromptTokens + ev.Usage.CompletionTokens
				}
				if total > 0 {
					tokensLabel.SetText(fmt.Sprintf("%d msgs · %d tokens", len(h.History()), total))
				}
			}
		case hub.EventAudioReady:
			// Last-active wins: only the originating subscriber plays. A
			// sentinel with empty AudioURL = "no audio this turn" — fall
			// through to the voice re-arm so continuous mode doesn't hang.
			if ev.Origin != desktopOrigin {
				break
			}
			armNext := func() {
				if lastDesktopWasVoice && settings.Load().ContinuousConversation {
					startListening()
				}
				lastDesktopWasVoice = false
			}
			if ev.AudioURL != "" {
				id := strings.TrimPrefix(ev.AudioURL, "/api/v1/audio/")
				if data, ok := h.Audio(id); ok && len(data) > 0 {
					if voice != nil {
						voice.Stop()
					}
					sp, err := tts.Play(context.Background(), data)
					if err != nil {
						setStatus("✗ TTS: " + err.Error())
						armNext()
						break
					}
					// OnDone fires on the TTS goroutine — marshal the re-arm.
					sp.OnDone = func() { fyne.Do(armNext) }
					voice = sp
					break
				}
			}
			armNext()
		case hub.EventPortraitProgress:
			// Intermediate ComfyUI preview frame. Missing previews are fine —
			// the next frame or the final EventPortraitReady lands soon.
			if ev.PortraitURL != "" {
				id := strings.TrimPrefix(ev.PortraitURL, "/api/v1/portrait-preview/")
				if data, ok := h.PortraitPreview(id); ok {
					portrait.show(data, false)
				}
			}
		case hub.EventPortraitReady:
			if ev.PortraitURL != "" {
				id := strings.TrimPrefix(ev.PortraitURL, "/api/v1/portrait/")
				if data, ok := h.Portrait(id); ok {
					portrait.show(data, true)
				}
			}
		case hub.EventTurnError:
			if ev.Origin == desktopOrigin {
				transcript.appendTurn("Error", ev.Error, labelUser)
				lastDesktopWasVoice = false
			}
			inFlight = false
			refreshInputEnabled()
		case hub.EventStatus:
			setStatus(ev.Status)
		case hub.EventCleared:
			transcript.clear()
			tokensLabel.SetText("")
		case hub.EventBusy:
			setStatus("Busy — wait for the current reply to finish.")
		}
	}

	// Drain pump. A goroutine ranges the hub's event channel and marshals each
	// event onto the UI thread — the Fyne replacement for the Qt QTimer drain.
	// The range exits when Unsubscribe closes the channel on shutdown.
	go func() {
		for ev := range desktopSub.Events {
			ev := ev
			fyne.Do(func() { dispatchEvent(ev) })
		}
	}()

	// ─── Menu ──────────────────────────────────────────────────────────────
	newConversation := func() {
		if transcript.empty && len(h.History()) == 0 {
			return
		}
		dialog.ShowConfirm("New Conversation",
			"Start a new conversation? This will erase the current chat history.",
			func(ok bool) {
				if !ok {
					return
				}
				if err := h.Clear(context.Background()); err != nil {
					setStatus("✗ " + err.Error())
				}
			}, win)
	}
	newItem := fyne.NewMenuItem("New Conversation", newConversation)
	newItem.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyN, Modifier: fyne.KeyModifierShortcutDefault}
	settingsItem := fyne.NewMenuItem("Settings…", func() {
		// Identity gates Send — re-evaluate after the dialog closes so saving
		// flips the input from disabled to enabled (or back) immediately.
		showSettingsDialog(win, srvMgr, smsMgr, tgMgr, mxMgr, refreshInputEnabled)
	})
	win.SetMainMenu(fyne.NewMainMenu(fyne.NewMenu("File", newItem, settingsItem)))

	// ─── Layout ────────────────────────────────────────────────────────────
	inputArea := container.NewVBox(
		container.NewBorder(nil, nil, nil, sendBtn, message),
		container.NewHBox(recordBtn, commitBtn),
	)
	convCol := container.NewBorder(
		widget.NewLabelWithStyle("Conversation", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		inputArea, nil, nil,
		transcript.object(),
	)
	body := container.NewBorder(nil, nil, nil, portrait.object(), convCol)
	win.SetContent(container.NewBorder(nil, footer, nil, nil, body))

	// Initial input gate — a fresh install without identity starts greyed with
	// the configure-me hint.
	refreshInputEnabled()

	win.ShowAndRun()

	// Teardown. Stop the server and bridges first so no in-flight handler
	// touches a half-torn-down hub, then drop the subscription (which closes
	// the drain channel) and stop the hub.
	srvMgr.Stop()
	smsMgr.Stop()
	tgMgr.Stop()
	mxMgr.Stop()
	h.Unsubscribe(desktopOrigin)
	h.Stop()
}
