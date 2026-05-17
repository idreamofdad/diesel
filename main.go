package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Transcript speaker-label colors. Blue for both sides of the conversation;
// a soft red for inline error rows so they're easy to spot without being
// alarming against the dark background.
const (
	labelBlue = "#5aa7ff"
	labelRed  = "#e57373"
)

func main() {
	// OpenTelemetry: a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT (or the
	// trace-specific override) is set in the environment. Shutdown flushes
	// any in-flight spans on exit; bound to a 5 s deadline so a stuck
	// collector can't hang the app indefinitely.
	if shutdown, err := initTracing(context.Background()); err != nil {
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

	qt.NewQApplication(os.Args)

	window := qt.NewQMainWindow(nil)
	window.SetWindowTitle("Diesel")
	window.Resize(960, 560)
	// Color palette matched to the Tk-rendered Python reference: dark grays
	// throughout, *gray* list selection (not the system blue), no harsh
	// borders. The native Qt dark style on macOS is close but uses system
	// accent colors for selection, which clashed with the reference.
	window.SetStyleSheet(`
		QMainWindow, QWidget { background-color: #2b2b2b; color: #ececec; }
		QLabel { color: #ececec; }

		QListWidget {
			background-color: #232323;
			border: 1px solid #3a3a3a;
			border-radius: 4px;
			outline: 0;
			padding: 4px;
		}
		QListWidget::item { padding: 5px 6px; border-radius: 3px; }
		QListWidget::item:selected,
		QListWidget::item:selected:!active {
			background-color: #3a3a3a;
			color: #ececec;
		}

		QTextEdit {
			background-color: #232323;
			color: #ececec;
			border: 1px solid #3a3a3a;
			border-radius: 4px;
			padding: 6px;
		}

		QLineEdit {
			background-color: #232323;
			color: #ececec;
			border: 1px solid #3a3a3a;
			border-radius: 4px;
			padding: 4px 6px;
			selection-background-color: #555;
		}

		QPushButton {
			background-color: #4a4a4a;
			color: #ececec;
			border: 1px solid #5a5a5a;
			border-radius: 5px;
			padding: 4px 12px;
		}
		QPushButton:hover  { background-color: #555; }
		QPushButton:pressed { background-color: #3f3f3f; }

		QProgressBar {
			background-color: #3a3a3a;
			border: 1px solid #3a3a3a;
			border-radius: 4px;
		}
		QProgressBar::chunk { background-color: #5a5a5a; border-radius: 4px; }
	`)

	central := qt.NewQWidget(nil)
	window.SetCentralWidget(central)

	outer := qt.NewQVBoxLayout(central)
	outer.SetContentsMargins(0, 0, 0, 0)
	outer.SetSpacing(0)

	// ─── Main two-pane area ────────────────────────────────────────────────
	body := qt.NewQWidget(nil)
	bodyLayout := qt.NewQHBoxLayout(body)
	bodyLayout.SetContentsMargins(14, 12, 14, 12)
	bodyLayout.SetSpacing(12)

	// Right column: the character portrait. ComfyUI renders a fresh one
	// after each reply when image generation is enabled (Settings); until
	// then the panel just shows a placeholder. Fixed width so the
	// transcript column gets all the slack. Built here, but added to the
	// body layout after the conversation column so it sits on the right.
	const portraitWidth = 300
	portraitCol := qt.NewQVBoxLayout2()
	portraitCol.SetSpacing(8)
	portraitHdr := qt.NewQLabel5("Diesel", nil)
	phFont := portraitHdr.Font()
	phFont.SetBold(true)
	portraitHdr.SetFont(phFont)
	portraitCol.AddWidget(portraitHdr.QWidget)
	portrait := qt.NewQLabel5("(no portrait yet)", nil)
	portrait.SetFixedWidth(portraitWidth)
	portrait.SetAlignment(qt.AlignCenter)
	portrait.SetStyleSheet(
		"background-color: #232323; border: 1px solid #3a3a3a;" +
			"border-radius: 4px; color: #888;")
	portrait.QWidget.SetSizePolicy2(qt.QSizePolicy__Fixed, qt.QSizePolicy__Preferred)
	portrait.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	portrait.SetToolTip("Double-click to view full size")
	portraitCol.AddWidget(portrait.QWidget)
	portraitCol.AddStretch()

	// latestPortraitPNG caches the bytes of the most recently shown
	// *final* portrait — not the intermediate diffusion previews — so the
	// double-click viewer can pop up the high-quality image without
	// re-reading from disk. Set from disk on startup and after each
	// successful render.
	var latestPortraitPNG []byte

	// showPortrait loads PNG bytes into the panel, scaled to the panel
	// width with the aspect ratio preserved. Empty or undecodable data
	// leaves whatever is already showing in place. `final` selects whether
	// these bytes should also feed the full-size viewer — preview frames
	// during generation pass false so the viewer keeps the previous final.
	showPortrait := func(png []byte, final bool) {
		if len(png) == 0 {
			return
		}
		pm := qt.NewQPixmap()
		if !pm.LoadFromDataWithData(png) || pm.IsNull() {
			return
		}
		if final {
			latestPortraitPNG = png
		}
		portrait.SetPixmap(pm.ScaledToWidth2(portraitWidth, qt.SmoothTransformation))
	}
	// Restore the last rendered portrait so the window opens with Diesel's
	// face already in place rather than the placeholder.
	if p, err := characterImagePath(); err == nil {
		if data, err := os.ReadFile(p); err == nil {
			showPortrait(data, true)
		}
	}

	// Double-click pops the full-resolution portrait in a modal viewer
	// dialog. We work from the cached bytes — re-decoding the original
	// PNG rather than upscaling the scaled-down thumbnail.
	portrait.OnMouseDoubleClickEvent(func(super func(event *qt.QMouseEvent), event *qt.QMouseEvent) {
		super(event)
		if len(latestPortraitPNG) == 0 {
			return
		}
		showPortraitFullSize(window.QWidget, latestPortraitPNG)
	})

	// Left column: header + transcript + input + media buttons.
	convCol := qt.NewQVBoxLayout2()
	convCol.SetSpacing(8)

	// Header: the "Conversation" title.
	convHdr := qt.NewQLabel5("Conversation", nil)
	hdrFont := convHdr.Font()
	hdrFont.SetBold(true)
	convHdr.SetFont(hdrFont)
	convCol.AddWidget(convHdr.QWidget)

	// Transcript area — read-only, with placeholder text shown when empty.
	transcript := qt.NewQTextEdit(nil)
	transcript.SetReadOnly(true)
	transcript.SetPlaceholderText("(Conversation will appear here)")
	convCol.AddWidget(transcript.QWidget)

	// Message input row.
	inputRow := qt.NewQHBoxLayout2()
	inputRow.SetSpacing(6)
	message := qt.NewQLineEdit(nil)
	message.SetPlaceholderText("Type a message...")
	sendBtn := qt.NewQPushButton3("Send")
	inputRow.AddWidget(message.QWidget)
	inputRow.AddWidget(sendBtn.QWidget)
	convCol.AddLayout(inputRow.QLayout)

	// Media controls row: record + upload buttons. Both get a round-ish
	// stylesheet to match the Python reference. AddStretch pushes them to
	// the left so they don't drift to the row's center when the window is
	// wide.
	mediaRow := qt.NewQHBoxLayout2()
	mediaRow.SetSpacing(8)
	roundBtnStyle := `
		QPushButton {
			background-color: #4a4a4a;
			border: 1px solid #5e5e5e;
			border-radius: 17px;
			color: white;
			font-size: 14px;
			padding: 0;
		}
		QPushButton:pressed { background-color: #5e5e5e; }
	`
	recordBtn := qt.NewQPushButton3("◉")
	recordBtn.SetFixedSize2(34, 34)
	recordBtn.SetStyleSheet(roundBtnStyle)
	uploadBtn := qt.NewQPushButton3("↑")
	uploadBtn.SetFixedSize2(34, 34)
	uploadBtn.SetStyleSheet(roundBtnStyle)
	mediaRow.AddWidget(recordBtn.QWidget)
	mediaRow.AddWidget(uploadBtn.QWidget)
	mediaRow.AddStretch()
	convCol.AddLayout(mediaRow.QLayout)

	// Conversation takes the slack; the portrait sits fixed-width on the right.
	bodyLayout.AddLayout2(convCol.QLayout, 1)
	bodyLayout.AddLayout(portraitCol.QLayout)
	outer.AddWidget2(body, 1)

	// Thin divider separating the body content from the status bar.
	divider := qt.NewQFrame(nil)
	divider.SetFrameShape(qt.QFrame__HLine)
	divider.SetFrameShadow(qt.QFrame__Plain)
	divider.SetFixedHeight(1)
	divider.SetStyleSheet("background-color: #3a3a3a; border: none;")
	outer.AddWidget(divider.QWidget)

	// ─── Status bar strip ─────────────────────────────────────────────────
	// Left-aligned status label, defaults to "Ready" until something
	// updates it. Roomy horizontal/vertical padding so the text breathes.
	footer := qt.NewQWidget(nil)
	footerLayout := qt.NewQHBoxLayout(footer)
	footerLayout.SetContentsMargins(14, 8, 14, 8)
	status := qt.NewQLabel5("Ready", nil)
	// Without this, a long error string would push the label's sizeHint
	// wide enough to force the whole window to grow. Ignored on the
	// horizontal axis tells the layout the label is happy at any width;
	// we hide overflow with an ellipsis in setStatus below.
	status.QWidget.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	status.QWidget.SetMinimumWidth(0)
	footerLayout.AddWidget2(status.QWidget, 1)

	// Right-aligned token gauge — populated from the `usage` block the
	// server returns on each completion. Stays blank until the first
	// reply lands; some local servers omit `usage` entirely, in which
	// case it just stays blank rather than showing a misleading 0.
	tokensLabel := qt.NewQLabel5("", nil)
	tokensLabel.SetStyleSheet("color: #888; font-size: 11px;")
	footerLayout.AddWidget(tokensLabel.QWidget)

	outer.AddWidget(footer)

	// setStatus updates the status bar with `msg`, eliding overflow with
	// an ellipsis rather than letting QLabel paint past its allotted
	// width. We remember the raw text so a window resize can re-elide
	// against the new width instead of permanently truncating.
	statusRaw := "Ready"
	applyStatus := func() {
		w := status.QWidget.Width()
		if w <= 0 {
			// Before the first layout pass the width is 0 — show the
			// raw text and let the resize event re-elide once Qt has
			// settled the geometry.
			status.SetText(statusRaw)
			return
		}
		fm := qt.NewQFontMetrics(status.Font())
		status.SetText(fm.ElidedText(statusRaw, qt.ElideRight, w))
	}
	setStatus := func(msg string) {
		statusRaw = msg
		applyStatus()
	}
	status.OnResizeEvent(func(super func(*qt.QResizeEvent), event *qt.QResizeEvent) {
		super(event)
		applyStatus()
	})

	// ─── Send wiring ──────────────────────────────────────────────────────
	// Triggered by either the Send button or Return in the message field.
	// Keeps an in-memory chat log alongside the visible transcript so we
	// can replay prior turns to the model on each call.
	//
	// The HTTP request runs in a goroutine while a QTimer on the main
	// thread polls a result channel. Running it inline on the UI thread
	// would freeze the event loop, which on macOS means the just-appended
	// "You:" paragraph wouldn't paint until the LLM replied — even with
	// ProcessEvents/Repaint, because Cocoa defers the display to its
	// compositor. Doing the work off-thread lets the Qt loop keep ticking
	// and paint normally.
	// History is restored from disk on launch (when "Save conversations to
	// disk" is enabled) so the window opens pre-populated with the previous
	// conversation. persistConversation mirrors the in-memory log back out
	// after every change; it's a no-op when the setting is off, and a
	// failed write only surfaces in the status bar — losing the transcript
	// is far worse than a stale save file.
	var history []chatMessage
	if loadSettings().SaveToDisk {
		history = loadConversation()
		for _, m := range history {
			switch m.Role {
			case roleUser:
				appendTurn(transcript, "You", m.Content, labelRed)
			case roleAssistant:
				appendTurn(transcript, "Diesel", m.Content, labelBlue)
			}
		}
	}
	// ctx threads the active turn span into the disk write so the save
	// nests under chat.turn instead of starting a new trace root. Callers
	// outside a turn pass context.Background().
	persistConversation := func(ctx context.Context) {
		if !loadSettings().SaveToDisk {
			return
		}
		if err := saveConversation(ctx, history); err != nil {
			setStatus("✗ Could not save conversation: " + err.Error())
		}
	}
	type chatResult struct {
		reply chatReply
		usage tokenUsage
		err   error
	}

	// Active TTS playback, if any. Replaced on each new assistant reply
	// and interrupted when the user starts recording (so Diesel doesn't
	// keep talking over the user's response).
	var voice *speaker
	// speakReply synthesizes `text` via the configured TTS endpoint and
	// plays it. Both the HTTP call and the playback are off the critical
	// path — TTS failure must never clobber the visible chat state, so
	// errors only surface via the status bar and the conversation keeps
	// flowing as if TTS wasn't there.
	type ttsResult struct {
		audio []byte
		err   error
	}
	// speakReply synthesizes and plays `text`, then calls onDone (when set)
	// exactly once on the Qt main thread. onDone fires after playback ends
	// naturally, or immediately when TTS is disabled / empty / fails — so a
	// caller chaining off it (continuous-conversation mode) advances no
	// matter which path TTS takes. `ctx` carries the parent turn span so the
	// emitted tts.synthesize span nests under chat.turn.
	speakReply := func(ctx context.Context, text string, onDone func()) {
		finish := func() {
			if onDone != nil {
				onDone()
			}
		}
		s := loadSettings()
		if !s.EnableTTS || strings.TrimSpace(text) == "" {
			finish()
			return
		}
		ep := firstNonEmpty(s.TTSEndpoint, s.APIEndpoint)
		key := firstNonEmpty(s.TTSAPIKey, s.APIKey)
		pollAsync(30,
			func() ttsResult {
				audio, err := synthesizeTTS(ctx, ep, key, s.TTSModel, s.TTSVoice, text)
				return ttsResult{audio, err}
			},
			func(r ttsResult) {
				if r.err != nil {
					setStatus("✗ TTS: " + r.err.Error())
					finish()
					return
				}
				// Replace any prior playback so two replies don't pile
				// up — easy to hit by retrying or sending fast.
				if voice != nil {
					voice.stop()
				}
				sp, err := playAudio(ctx, r.audio)
				if err != nil {
					setStatus("✗ TTS: " + err.Error())
					finish()
					return
				}
				// finish() runs when playback drains on its own, not when
				// a later stop() cancels it — see speaker.onDone.
				sp.onDone = finish
				voice = sp
			})
	}

	// generatePortrait renders a fresh character portrait via ComfyUI and
	// drops it into the side panel. Like speakReply it's best-effort and
	// off the critical path — a render failure only touches the status bar,
	// the chat keeps flowing. A no-op unless image generation is enabled in
	// Settings. generateImage picks a new random seed each call, so calling
	// this after every reply yields a new image rather than the same one.
	//
	// `emotion` (from the structured chat reply) is spliced into the image
	// prompt by looking up emotionPrompts so the portrait matches the
	// reply's mood. Empty/neutral falls through unchanged. `naked` is the
	// per-turn nudity flag from the same reply; when true it appends
	// nudityPrompt so the renderer drops the character's clothing.
	//
	// Streaming: generateImage reports step progress and preview frames
	// over a buffered channel; the QTimer drains both, repainting the
	// portrait pane as preview frames arrive. Step counts are received
	// but currently unused — kept on the channel in case we want to show
	// them again later.
	type imageResult struct {
		png []byte
		err error
	}
	// `ctx` carries the parent turn span so the image.generate span nests
	// under chat.turn. `onDone` fires exactly once (on the Qt main thread)
	// when the render finishes — success, failure, or the no-op "image gen
	// disabled" early return — so the caller can clear an outstanding-work
	// counter for the turn span without caring which path was taken.
	generatePortrait := func(ctx context.Context, emotion string, naked bool, onDone func()) {
		finish := func() {
			if onDone != nil {
				onDone()
			}
		}
		s := loadSettings()
		if !s.EnableImageGen {
			finish()
			return
		}
		prompt := strings.TrimSpace(s.ImagePrompt)
		// Clothing and nudity are mutually exclusive — splicing both would
		// pull the renderer in two directions. naked=true wins. Both
		// strings come from Settings so the user can retune them; either
		// blank means "skip the splice".
		switch {
		case naked:
			if frag := strings.TrimSpace(s.ImageNudity); frag != "" {
				prompt = prompt + ", " + frag
			}
		default:
			if frag := strings.TrimSpace(s.ImageClothing); frag != "" {
				prompt = prompt + ", " + frag
			}
		}
		if frag := emotionPrompts[strings.TrimSpace(emotion)]; frag != "" {
			prompt = prompt + ", " + frag
		}
		setStatus("Rendering portrait…")

		// Buffer the progress channel generously: preview frames are
		// large and arrive faster than the QTimer drains. Dropping the
		// odd preview is fine; missing the final image is not — that
		// goes through `done` instead.
		done := make(chan imageResult, 1)
		progress := make(chan imageProgress, 32)
		go func() {
			png, err := generateImage(ctx, s, prompt, s.ImageNegativePrompt, func(p imageProgress) {
				select {
				case progress <- p:
				default:
					// Buffer full — drop. UI catches up on next event.
				}
			})
			done <- imageResult{png, err}
		}()
		// Tighter poll than the previous 250ms because previews come in
		// every step or two and the UI feels laggy otherwise.
		poller := qt.NewQTimer()
		poller.SetSingleShot(false)
		poller.OnTimeout(func() {
			// Drain any queued progress events first so the most recent
			// preview / step counter wins this tick.
		drain:
			for {
				select {
				case p := <-progress:
					if len(p.Preview) > 0 {
						showPortrait(p.Preview, false)
					}
				default:
					break drain
				}
			}
			select {
			case r := <-done:
				poller.Stop()
				if r.err != nil {
					setStatus("✗ Portrait: " + r.err.Error())
					finish()
					return
				}
				showPortrait(r.png, true)
				if err := saveCharacterImage(r.png); err != nil {
					setStatus("✗ Could not cache portrait: " + err.Error())
					finish()
					return
				}
				setStatus("Ready")
				finish()
			default:
			}
		})
		poller.Start(80)
	}

	// sendMessage and startListening are mutually recursive — a hands-free
	// voice turn flows sendMessage → reply → startListening → transcribe →
	// sendMessage — so both are declared up front and assigned below.
	// sendMessage's viaVoice flag records whether the turn originated from
	// the mic; only voice turns re-arm the continuous-conversation loop.
	var sendMessage func(viaVoice bool)
	var startListening func()
	sendMessage = func(viaVoice bool) {
		text := strings.TrimSpace(message.Text())
		if text == "" {
			return
		}
		message.Clear()
		appendTurn(transcript, "You", text, labelRed)
		history = append(history, chatMessage{Role: roleUser, Content: text, Timestamp: time.Now()})

		sendBtn.SetEnabled(false)
		message.SetEnabled(false)
		setStatus("Sending…")

		// Snapshot history + settings so the goroutine never reads state
		// the main thread might mutate. (Input is disabled, but defensive.)
		s := loadSettings()
		snapshot := append([]chatMessage(nil), history...)

		// Parent span for the whole turn — covers chat, TTS, and portrait so
		// a single trace shows the user-perceived latency end-to-end. Ends
		// only after every child operation reports done; `pending` tracks
		// the outstanding count and is mutated only from the Qt main thread
		// (every callback below runs there).
		turnCtx, turnSpan := startSpan(context.Background(), "chat.turn",
			attribute.Bool("turn.via_voice", viaVoice),
			attribute.Bool("turn.continuous", s.ContinuousConversation),
			attribute.Int("turn.history.messages", len(snapshot)),
		)
		pending := 1
		endChild := func() {
			pending--
			if pending == 0 {
				turnSpan.End()
			}
		}

		pollAsync(30,
			func() chatResult {
				reply, usage, err := chatCompletion(turnCtx, s, snapshot)
				return chatResult{reply, usage, err}
			},
			func(r chatResult) {
				sendBtn.SetEnabled(true)
				message.SetEnabled(true)
				message.QWidget.SetFocus()
				if r.err != nil {
					setStatus("✗ " + r.err.Error())
					appendTurn(transcript, "Error", r.err.Error(), labelRed)
					// Drop the user turn so the next send isn't replayed
					// with a half-finished exchange in history.
					history = history[:len(history)-1]
					persistConversation(turnCtx)
					turnSpan.RecordError(r.err)
					turnSpan.SetStatus(codes.Error, r.err.Error())
					endChild()
					return
				}
				// Only the text goes into history/transcript — the emotion
				// is consumed locally by the portrait pipeline and isn't
				// worth replaying back to the model on the next turn.
				history = append(history, chatMessage{Role: roleAssistant, Content: r.reply.Text, Timestamp: time.Now()})
				appendTurn(transcript, "Diesel", r.reply.Text, labelBlue)
				persistConversation(turnCtx)
				setStatus("Ready")
				// Prefer the server-reported total; fall back to summing
				// the prompt+completion fields when only those are set
				// (some llama.cpp builds report the parts but not the
				// total). Skip the update entirely when the server didn't
				// include a usage block — leaving the prior value in
				// place reads better than flashing to 0.
				total := r.usage.TotalTokens
				if total == 0 {
					total = r.usage.PromptTokens + r.usage.CompletionTokens
				}
				if total > 0 {
					tokensLabel.SetText(fmt.Sprintf("%d msgs · %d tokens", len(history), total))
				}
				// Continuous-conversation mode reopens the mic once the
				// reply finishes being spoken — but only for voice turns,
				// so typing a message never silently arms the mic.
				var afterSpeak func()
				if s.ContinuousConversation && viaVoice {
					afterSpeak = startListening
				}
				// Bump pending for TTS + portrait before kicking either
				// off — both callbacks could fire synchronously on the
				// disabled/no-op paths, and we need the count to be
				// accurate before that happens.
				pending += 2
				speakReply(turnCtx, r.reply.Text, func() {
					if afterSpeak != nil {
						afterSpeak()
					}
					endChild()
				})
				// Refresh Diesel's portrait to match the reply's mood —
				// the emotion is spliced into the image prompt below.
				generatePortrait(turnCtx, r.reply.Emotion, r.reply.Naked, endChild)
				endChild() // chat reply done
			})
	}
	sendBtn.OnClicked(func() { sendMessage(false) })
	message.OnReturnPressed(func() { sendMessage(false) })

	// ─── Record button + VAD ──────────────────────────────────────────────
	// Press once to start capturing from the default microphone. The VAD
	// auto-stops on trailing silence (or a hard 30 s ceiling); a second
	// press cancels mid-recording. On stop we POST the audio to the
	// configured STT endpoint and, on success, auto-send the transcript
	// as a chat turn so the workflow stays hands-free.
	var rec *recorder
	const (
		recordGlyphIdle   = "◉"
		recordGlyphActive = "⏹"
	)
	type sttResult struct {
		text string
		err  error
	}
	setRecordingUI := func(active bool) {
		if active {
			recordBtn.SetText(recordGlyphActive)
			// Upload button doubles as the commit/send action while a
			// recording is in flight: ⏹ cancels, ↑ commits. Both stay
			// enabled so the user always has both choices.
			uploadBtn.SetEnabled(true)
			message.SetEnabled(false)
			sendBtn.SetEnabled(false)
		} else {
			recordBtn.SetText(recordGlyphIdle)
			uploadBtn.SetEnabled(true)
			message.SetEnabled(true)
			sendBtn.SetEnabled(true)
		}
	}
	// startListening opens the mic and wires up the VAD. Extracted from the
	// record button so continuous-conversation mode can reopen the mic with
	// no click — it's a no-op if a recording is already in progress.
	startListening = func() {
		if rec != nil {
			return
		}
		// Don't let Diesel talk over the user. If a previous reply is
		// still being spoken, stop it before we open the mic — otherwise
		// the VAD picks up the playback and trims the user's speech.
		if voice != nil {
			voice.stop()
			voice = nil
		}
		r, err := startRecording(context.Background(), func(pcm []byte, reason stopReason) {
			// onStop runs on the Qt main thread, either from VAD or
			// from a manual cancel. Reset UI first, then decide
			// whether to transcribe.
			rec = nil
			setRecordingUI(false)
			if reason == stopCancelled {
				setStatus("Ready")
				return
			}
			if reason == stopNoSpeech {
				setStatus("No speech detected")
				return
			}
			// stopVAD / stopMaxLength / stopCommitted all fall through to
			// transcription — the audio is wanted.
			setStatus("Transcribing…")
			s := loadSettings()
			ep := firstNonEmpty(s.STTEndpoint, s.APIEndpoint)
			key := firstNonEmpty(s.STTAPIKey, s.APIKey)
			wav := encodeWAV(pcm)
			pollAsync(30,
				func() sttResult {
					t, err := transcribe(context.Background(), ep, key, s.STTModel, wav)
					return sttResult{t, err}
				},
				func(r sttResult) {
					if r.err != nil {
						setStatus("✗ " + r.err.Error())
						return
					}
					if strings.TrimSpace(r.text) == "" {
						setStatus("No speech detected")
						return
					}
					message.SetText(r.text)
					// viaVoice=true: this turn came from the mic, so it
					// re-arms the continuous-conversation loop.
					sendMessage(true)
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
	recordBtn.OnClicked(func() {
		if rec != nil {
			// Already recording → treat as cancel. The onStop callback
			// will reset UI state.
			rec.stop(stopCancelled)
			return
		}
		startListening()
	})
	// Upload button's role during recording is "commit and send" — it
	// stops the recording with stopCommitted, which the onStop handler
	// above treats the same as a VAD-driven stop (transcribe + auto-send).
	// When no recording is active the button is reserved for file upload,
	// which isn't wired yet, so it's a no-op for now.
	uploadBtn.OnClicked(func() {
		if rec != nil {
			rec.stop(stopCommitted)
		}
	})

	// ─── Menu bar ─────────────────────────────────────────────────────────
	// macOS reroutes this into the native menu bar at the screen top; on
	// Linux/Windows it renders inside the window.
	mb := window.MenuBar()
	fileMenu := mb.AddMenuWithTitle("File")

	newAction := fileMenu.AddAction3(
		"New Conversation",
		qt.NewQKeySequence6(qt.QKeySequence__New), // Cmd+N / Ctrl+N
	)
	// Diesel holds a single conversation, so "New" means discarding the
	// current one. Confirm first — the transcript and in-memory history are
	// both wiped, with no undo.
	newAction.OnTriggered(func() {
		if transcript.ToPlainText() == "" && len(history) == 0 {
			return // nothing to erase
		}
		answer := qt.QMessageBox_Question6(
			window.QWidget,
			"New Conversation",
			"Start a new conversation? This will erase the current chat history.",
			qt.QMessageBox__Yes|qt.QMessageBox__Cancel,
			qt.QMessageBox__Cancel,
		)
		if answer != qt.QMessageBox__Yes {
			return
		}
		transcript.Clear()
		history = nil
		persistConversation(context.Background())
		tokensLabel.SetText("")
		setStatus("Ready")
	})

	// Settings — flagged with PreferencesRole so Qt moves it to the
	// application menu on macOS (the "diesel" / "Audio App" menu next to
	// the Apple logo). On other platforms it stays under File.
	prefsAction := qt.NewQAction2("Settings…")
	prefsAction.SetMenuRole(qt.QAction__PreferencesRole)
	prefsAction.OnTriggered(func() {
		showSettingsDialog(window.QWidget)
	})
	fileMenu.AddAction(prefsAction)

	window.Show()
	qt.QApplication_Exec()
}

// showPortraitFullSize pops a modal viewer with the portrait scaled to
// fill the screen's available height. The dialog's width follows from the
// image's aspect ratio at that height, so the window stays just as wide
// as the picture itself — no letterboxing, no extra chrome around it.
func showPortraitFullSize(parent *qt.QWidget, png []byte) {
	pm := qt.NewQPixmap()
	if !pm.LoadFromDataWithData(png) || pm.IsNull() {
		return
	}

	w, h := pm.Width(), pm.Height()
	if screen := qt.QGuiApplication_PrimaryScreen(); screen != nil && h > 0 {
		avail := screen.AvailableGeometry()
		// Scale by height to fill the screen vertically; width tracks
		// the aspect ratio. Clamp if that makes the dialog wider than
		// the screen (very wide / panoramic images).
		w = w * avail.Height() / h
		h = avail.Height()
		if w > avail.Width() {
			h = h * avail.Width() / w
			w = avail.Width()
		}
		pm = pm.Scaled3(w, h, qt.KeepAspectRatio, qt.SmoothTransformation)
		// Use the post-scale pixmap dimensions so the dialog matches
		// the image exactly — Scaled3 with KeepAspectRatio can round
		// off a pixel or two, which leaves a 1px gap otherwise.
		w, h = pm.Width(), pm.Height()
	}

	// Popup window type: frameless, no taskbar entry, and auto-closes
	// when the user clicks anywhere outside it — which is the exact
	// dismissal behavior we want here. FramelessWindowHint and
	// CustomizeWindowHint are kept as belt-and-suspenders so Qt can't
	// sneak any default title chrome back in on macOS. The label is
	// parented directly to the dialog with absolute geometry rather
	// than via a QVBoxLayout — no layout we tried reliably zeroed out
	// the top gap. Stylesheet override + SetFixedSize keep any residual
	// gap from rendering as a visible grey bar against the inherited
	// dark theme. Escape (QDialog default) and a click inside both
	// close it too.
	dlg := qt.NewQDialog(parent)
	dlg.SetWindowFlags(qt.Popup | qt.FramelessWindowHint | qt.CustomizeWindowHint)
	dlg.SetWindowTitle("Diesel")
	dlg.SetStyleSheet("QDialog, QLabel { background-color: #000; border: 0; margin: 0; padding: 0; }")

	img := qt.NewQLabel5("", dlg.QWidget)
	img.SetPixmap(pm)
	img.SetGeometry(0, 0, w, h)

	dlg.OnMousePressEvent(func(super func(event *qt.QMouseEvent), event *qt.QMouseEvent) {
		super(event)
		dlg.Accept()
	})

	dlg.SetFixedSize2(w, h)
	dlg.Exec()
}

// showSettingsDialog presents a modal settings dialog populated from the
// on-disk settings file. Save writes them back; Cancel discards.
func showSettingsDialog(parent *qt.QWidget) {
	current := loadSettings()

	dlg := qt.NewQDialog(parent)
	dlg.SetWindowTitle("Settings")
	dlg.Resize(500, 600)

	root := qt.NewQVBoxLayout(dlg.QWidget)
	root.SetContentsMargins(18, 18, 18, 14)
	root.SetSpacing(12)

	// Each settings tab is a QFormLayout hosted on a QWidget, wrapped in a
	// QScrollArea so tall content (the prompt editors, mainly) can grow
	// past the dialog height and the user scrolls instead of the dialog
	// stretching. SetWidgetResizable(true) lets the inner widget track the
	// viewport width — without it, fields would clip horizontally instead
	// of growing to fill the tab. Spacing and growth policy are shared so
	// non-fixed fields fill the available horizontal space.
	newTab := func() (*qt.QFormLayout, *qt.QWidget) {
		f := qt.NewQFormLayout2()
		f.SetSpacing(10)
		f.SetContentsMargins(14, 14, 14, 14)
		f.SetFieldGrowthPolicy(qt.QFormLayout__AllNonFixedFieldsGrow)
		inner := qt.NewQWidget(nil)
		inner.SetLayout(f.QLayout)
		scroll := qt.NewQScrollArea(nil)
		scroll.SetWidget(inner)
		scroll.SetWidgetResizable(true)
		return f, scroll.QWidget
	}

	// Appearance.
	theme := qt.NewQComboBox(nil)
	theme.AddItems([]string{"System", "Dark", "Light"})
	setComboSelection(theme, current.Theme)

	// API.
	endpoint := qt.NewQLineEdit3(current.APIEndpoint)
	apiKey := qt.NewQLineEdit3(current.APIKey)
	apiKey.SetEchoMode(qt.QLineEdit__Password)
	apiKey.SetPlaceholderText("sk-…")

	// Model selectors share a fixed width so the form stays stable: an
	// editable combo otherwise sizes itself to its longest entry, and
	// model IDs vary wildly in length between providers.
	const modelComboWidth = 280

	// The model list is populated live from the configured provider rather
	// than hardcoded. Editable so the user can still type a custom name if
	// the server doesn't advertise the one they want.
	model := qt.NewQComboBox(nil)
	model.SetEditable(true)
	model.SetFixedWidth(modelComboWidth)
	loadModels := func(ep, key string) {
		populateModelCombo(model, current.Model, func() ([]string, error) {
			return fetchModels(ep, key)
		})
	}
	loadModels(current.APIEndpoint, current.APIKey)

	// Audio devices, enumerated from Qt's multimedia subsystem. "System
	// Default" stays as the first option (and the fallback) so users
	// don't have to pick a specific device just to get going. The output
	// list is also enumerated for symmetry, even though we don't play
	// audio yet — output device wiring is a follow-up.
	inputDevice := qt.NewQComboBox(nil)
	inputDevice.AddItem("System Default")
	for _, d := range audioInputDescriptions() {
		inputDevice.AddItem(d)
	}
	setComboSelection(inputDevice, current.InputDevice)
	outputDevice := qt.NewQComboBox(nil)
	outputDevice.AddItem("System Default")
	for _, d := range audioOutputDescriptions() {
		outputDevice.AddItem(d)
	}
	setComboSelection(outputDevice, current.OutputDevice)

	// Speech-to-text settings. All three fields are optional and fall
	// back to their LLM counterparts at request time, so a user pointing
	// a single Speaches/LM-Studio server at both can leave them blank.
	sttEndpoint := qt.NewQLineEdit3(current.STTEndpoint)
	sttEndpoint.SetPlaceholderText("(falls back to API endpoint)")
	sttAPIKey := qt.NewQLineEdit3(current.STTAPIKey)
	sttAPIKey.SetEchoMode(qt.QLineEdit__Password)
	sttAPIKey.SetPlaceholderText("(falls back to API key)")
	// Editable combo so the user can still type a custom name when the
	// server doesn't enumerate one. Populated live from the STT endpoint
	// (or the LLM endpoint when STT is left blank) — Speaches advertises
	// ASR models via /v1/models, which is enough to fill the dropdown.
	sttModel := qt.NewQComboBox(nil)
	sttModel.SetEditable(true)
	sttModel.SetFixedWidth(modelComboWidth)
	sttModel.LineEdit().SetPlaceholderText("whisper-1")
	// loadSTTModels falls back to the LLM endpoint/key when the STT-specific
	// ones are blank — mirrors the precedence used at record time, so the
	// dropdown shows what would actually be queried.
	loadSTTModels := func(ep, key string) {
		ep = firstNonEmpty(ep, endpoint.Text())
		key = firstNonEmpty(key, apiKey.Text())
		populateModelCombo(sttModel, current.STTModel, func() ([]string, error) {
			return fetchSTTModels(ep, key)
		})
	}
	loadSTTModels(current.STTEndpoint, current.STTAPIKey)
	// Debounced refresh — same shape as the system-prompt token counter
	// above. Triggered when the user edits either the STT endpoint/key
	// directly, or the LLM endpoint/key while the STT ones are blank
	// (because the loader falls back to those).
	sttModelTimer := qt.NewQTimer()
	sttModelTimer.SetSingleShot(true)
	sttModelTimer.OnTimeout(func() {
		loadSTTModels(sttEndpoint.Text(), sttAPIKey.Text())
	})
	sttEndpoint.OnTextChanged(func(string) { sttModelTimer.Start(400) })
	sttAPIKey.OnTextChanged(func(string) { sttModelTimer.Start(400) })

	// Text-to-speech settings. Symmetric to STT, plus a Voice field —
	// every modern TTS model serves multiple voices, and the choice is
	// per-request rather than baked into the model.
	enableTTS := qt.NewQCheckBox3("Speak replies through TTS")
	enableTTS.SetChecked(current.EnableTTS)
	ttsEndpoint := qt.NewQLineEdit3(current.TTSEndpoint)
	ttsEndpoint.SetPlaceholderText("(falls back to API endpoint)")
	ttsAPIKey := qt.NewQLineEdit3(current.TTSAPIKey)
	ttsAPIKey.SetEchoMode(qt.QLineEdit__Password)
	ttsAPIKey.SetPlaceholderText("(falls back to API key)")
	ttsModel := qt.NewQComboBox(nil)
	ttsModel.SetEditable(true)
	ttsModel.SetFixedWidth(modelComboWidth)
	ttsModel.LineEdit().SetPlaceholderText("tts-1")
	loadTTSModels := func(ep, key string) {
		ep = firstNonEmpty(ep, endpoint.Text())
		key = firstNonEmpty(key, apiKey.Text())
		populateModelCombo(ttsModel, current.TTSModel, func() ([]string, error) {
			return fetchTTSModels(ep, key)
		})
	}
	loadTTSModels(current.TTSEndpoint, current.TTSAPIKey)
	ttsModelTimer := qt.NewQTimer()
	ttsModelTimer.SetSingleShot(true)
	ttsModelTimer.OnTimeout(func() {
		loadTTSModels(ttsEndpoint.Text(), ttsAPIKey.Text())
	})
	ttsEndpoint.OnTextChanged(func(string) { ttsModelTimer.Start(400) })
	ttsAPIKey.OnTextChanged(func(string) { ttsModelTimer.Start(400) })
	ttsVoice := qt.NewQLineEdit3(current.TTSVoice)
	ttsVoice.SetPlaceholderText("alloy")

	// System prompt — multi-line, sent as the leading "system" message in
	// every LLM call. QTextEdit (rather than QLineEdit) so long prompts
	// stay readable. No max height so it expands to fill whatever vertical
	// space the LLM tab has spare; the minimum keeps ~4 lines visible even
	// on a tiny dialog.
	systemPrompt := qt.NewQTextEdit(nil)
	systemPrompt.SetPlaceholderText("Instructions sent to the model before each conversation…")
	systemPrompt.SetPlainText(current.SystemPrompt)
	systemPrompt.SetMinimumHeight(96)

	// Approximate token count, right-aligned under the editor. Pure local
	// chars/4 heuristic — no server roundtrip, no async upgrade. An earlier
	// version called the server's tokenize endpoint and fell back to this
	// estimate on failure, but the exact-count path sometimes returned 0
	// for non-empty input and caused the label to flicker to "0 tokens"
	// mid-edit. A pure estimate is stable, instant, and accurate enough
	// for sizing a prompt at a glance.
	tokenCount := qt.NewQLabel5("", nil)
	tokenCount.SetStyleSheet("color: #888; font-size: 11px;")
	tokenCount.SetAlignment(qt.AlignRight | qt.AlignVCenter)
	systemPromptCol := qt.NewQVBoxLayout2()
	systemPromptCol.SetSpacing(2)
	systemPromptCol.AddWidget(systemPrompt.QWidget)
	systemPromptCol.AddWidget(tokenCount.QWidget)
	updateTokenCount := func() {
		n := estimateTokens(systemPrompt.ToPlainText())
		if n == 0 {
			tokenCount.SetText("0 tokens")
		} else {
			tokenCount.SetText(fmt.Sprintf("~%d tokens", n))
		}
	}
	systemPrompt.OnTextChanged(updateTokenCount)
	updateTokenCount()

	// Context window — read-only, fetched from the server. OpenAI-compat
	// endpoints don't expose this, so fetchModelContextLength probes each
	// known backend's native endpoint (LM Studio /api/v0/models, llama.cpp
	// /props, Ollama /api/show). A debounce timer coalesces the rapid
	// refreshes triggered by typing in endpoint/key/model.
	contextLabel := qt.NewQLabel5("—", nil)
	contextLabel.SetStyleSheet("color: #888;")
	refreshContext := func() {
		ep, key, mid := endpoint.Text(), apiKey.Text(), model.CurrentText()
		if strings.TrimSpace(ep) == "" || strings.TrimSpace(mid) == "" {
			contextLabel.SetText("—")
			return
		}
		contextLabel.SetText("Probing…")
		pollAsync(60, func() int {
			return fetchModelContextLength(ep, key, mid)
		}, func(n int) {
			if n <= 0 {
				contextLabel.SetText("not reported by this server")
				return
			}
			contextLabel.SetText(fmt.Sprintf("%d tokens", n))
		})
	}
	contextTimer := qt.NewQTimer()
	contextTimer.SetSingleShot(true)
	contextTimer.OnTimeout(refreshContext)
	model.OnCurrentTextChanged(func(string) { contextTimer.Start(400) })
	// Kick off an initial probe so the label populates without waiting for
	// the user to touch any field. Goes through the timer so a flurry of
	// programmatic field assignments (none yet, but cheap insurance) collapses.
	contextTimer.Start(0)

	// Refetch the STT/TTS model dropdowns when the LLM endpoint/key change,
	// but only while the STT/TTS-specific fields are blank — that's when
	// loadSTTModels / loadTTSModels fall back to these as the source. The
	// context-length probe always refreshes since it follows the LLM
	// endpoint/key directly.
	endpoint.OnTextChanged(func(string) {
		if strings.TrimSpace(sttEndpoint.Text()) == "" {
			sttModelTimer.Start(400)
		}
		if strings.TrimSpace(ttsEndpoint.Text()) == "" {
			ttsModelTimer.Start(400)
		}
		contextTimer.Start(400)
	})
	apiKey.OnTextChanged(func(string) {
		if strings.TrimSpace(sttAPIKey.Text()) == "" {
			sttModelTimer.Start(400)
		}
		if strings.TrimSpace(ttsAPIKey.Text()) == "" {
			ttsModelTimer.Start(400)
		}
		contextTimer.Start(400)
	})

	// History length. 0 means "no history; only send the latest message".
	historyMessages := qt.NewQSpinBox(nil)
	historyMessages.SetRange(0, 500)
	historyMessages.SetSingleStep(1)
	historyMessages.SetSuffix(" messages")
	historyMessages.SetValue(current.HistoryMessages)

	// Behavior.
	autoSave := qt.NewQCheckBox3("Save conversations to disk")
	autoSave.SetChecked(current.SaveToDisk)

	// Continuous conversation: after a spoken turn gets a reply, reopen the
	// mic automatically so a voice chat flows hands-free. Typed turns are
	// unaffected. Lives in the STT section because it governs the listening
	// loop, even though the reply that triggers it may be spoken via TTS.
	continuousConv := qt.NewQCheckBox3("Continuous conversation (keep listening after each reply)")
	continuousConv.SetChecked(current.ContinuousConversation)

	// makeTestRow builds a {button | status-label} row for the bottom of
	// each section. The button reads the *current* field values so users
	// can try edits without saving first.
	makeTestRow := func(label string) (*qt.QHBoxLayout, *qt.QPushButton, *qt.QLabel) {
		row := qt.NewQHBoxLayout2()
		btn := qt.NewQPushButton3(label)
		status := qt.NewQLabel5("", nil)
		status.SetWordWrap(true)
		row.AddWidget(btn.QWidget)
		row.AddWidget2(status.QWidget, 1)
		return row, btn, status
	}

	// LLM test: probe /models on the configured endpoint and report what
	// came back. Sync HTTP call; ProcessEvents pumps the loop so the
	// "Testing…" state actually paints before the call blocks the UI thread.
	llmTestRow, llmTestBtn, llmTestStatus := makeTestRow("Test connection")
	llmTestBtn.OnClicked(func() {
		llmTestBtn.SetEnabled(false)
		llmTestStatus.SetText("Testing…")
		qt.QCoreApplication_ProcessEvents()
		result := testLLMConnection(endpoint.Text(), apiKey.Text())
		llmTestStatus.SetText(result)
		// On success, refresh the model list against the just-validated
		// endpoint/key so the user sees the new options without reopening.
		// The STT/TTS pickers refresh alongside it because both fall back
		// to the same endpoint/key when their dedicated fields are blank.
		// Context-length probe re-runs too so it tracks the freshly-loaded
		// model state.
		if strings.HasPrefix(result, "✓") {
			loadModels(endpoint.Text(), apiKey.Text())
			loadSTTModels(sttEndpoint.Text(), sttAPIKey.Text())
			loadTTSModels(ttsEndpoint.Text(), ttsAPIKey.Text())
			refreshContext()
		}
		llmTestBtn.SetEnabled(true)
	})

	// STT test: probe the STT endpoint's /models list, mirroring the
	// runtime fallback to the LLM endpoint/key when the STT-specific
	// fields are blank. A reachable /models is necessary but not
	// sufficient for transcription to work — a real round-trip test
	// would need to record audio, which isn't worth wiring into a
	// settings dialog.
	sttTestRow, sttTestBtn, sttTestStatus := makeTestRow("Test connection")
	sttTestBtn.OnClicked(func() {
		sttTestBtn.SetEnabled(false)
		sttTestStatus.SetText("Testing…")
		qt.QCoreApplication_ProcessEvents()
		ep := firstNonEmpty(sttEndpoint.Text(), endpoint.Text())
		key := firstNonEmpty(sttAPIKey.Text(), apiKey.Text())
		if strings.TrimSpace(ep) == "" {
			sttTestStatus.SetText("✗ No endpoint configured.")
		} else if ids, err := fetchSTTModels(ep, key); err != nil {
			sttTestStatus.SetText("✗ " + err.Error())
		} else if len(ids) == 0 {
			sttTestStatus.SetText("✓ Connected, but the server returned no models.")
		} else {
			sttTestStatus.SetText(fmt.Sprintf("✓ Connected — %d model(s) available.", len(ids)))
			// Refresh the dropdown to match the just-validated server.
			loadSTTModels(sttEndpoint.Text(), sttAPIKey.Text())
		}
		sttTestBtn.SetEnabled(true)
	})

	// TTS test: synthesize a short phrase and play it through the
	// configured output device. Doubles as a voice preview — far more
	// useful than another /models probe, because it verifies the model
	// name, the voice name, the synthesis pipeline, and audio playback
	// all line up. testVoice tracks the playback so a re-click cleanly
	// replaces it instead of stacking two phrases on top of each other.
	var testVoice *speaker
	ttsTestRow, ttsTestBtn, ttsTestStatus := makeTestRow("Test voice")
	ttsTestBtn.OnClicked(func() {
		ttsTestBtn.SetEnabled(false)
		ttsTestStatus.SetText("Synthesizing…")
		qt.QCoreApplication_ProcessEvents()
		if testVoice != nil {
			testVoice.stop()
			testVoice = nil
		}
		ep := firstNonEmpty(ttsEndpoint.Text(), endpoint.Text())
		key := firstNonEmpty(ttsAPIKey.Text(), apiKey.Text())
		if strings.TrimSpace(ep) == "" {
			ttsTestStatus.SetText("✗ No endpoint configured.")
			ttsTestBtn.SetEnabled(true)
			return
		}
		audio, err := synthesizeTTS(context.Background(), ep, key, ttsModel.CurrentText(), ttsVoice.Text(),
			"Testing, one two three.")
		if err != nil {
			ttsTestStatus.SetText("✗ " + err.Error())
			ttsTestBtn.SetEnabled(true)
			return
		}
		sp, err := playAudio(context.Background(), audio)
		if err != nil {
			ttsTestStatus.SetText("✗ " + err.Error())
			ttsTestBtn.SetEnabled(true)
			return
		}
		testVoice = sp
		ttsTestStatus.SetText("✓ Speaking…")
		ttsTestBtn.SetEnabled(true)
	})
	// Stop any in-flight test playback when the dialog closes — otherwise
	// the speaker keeps running past Cancel/Save.
	dlg.OnFinished(func(int) {
		if testVoice != nil {
			testVoice.stop()
			testVoice = nil
		}
	})

	// Image generation (ComfyUI). There's no model picker — the checkpoint
	// and every other model are baked into the bundled workflow. The prompt
	// is multi-line (like the system prompt); the negative prompt is
	// single-line since it's usually a short tag list.
	enableImageGen := qt.NewQCheckBox3("Render a character portrait after each reply")
	enableImageGen.SetChecked(current.EnableImageGen)
	comfyEndpoint := qt.NewQLineEdit3(current.ComfyUIEndpoint)
	comfyEndpoint.SetPlaceholderText("http://127.0.0.1:8188")

	// Both prompt editors are QTextEdits with a matching minimum height —
	// in a QFormLayout, two rows that both have an expanding vertical size
	// policy split the spare vertical space evenly, so they stay the same
	// size as the tab grows.
	imagePromptEdit := qt.NewQTextEdit(nil)
	imagePromptEdit.SetPlaceholderText("How Diesel should look…")
	imagePromptEdit.SetPlainText(current.ImagePrompt)
	imagePromptEdit.SetMinimumHeight(240)
	// Clothing and nudity are paired fields: the portrait pipeline
	// splices one or the other depending on the chat reply's Naked
	// flag. Multi-line QTextEdits sized for ~3 visible lines so the
	// user can write longer tag lists without horizontal scrolling.
	imageClothingEdit := qt.NewQTextEdit(nil)
	imageClothingEdit.SetPlaceholderText("e.g. wearing a blue t-shirt and blue jeans")
	imageClothingEdit.SetPlainText(current.ImageClothing)
	imageClothingEdit.SetMinimumHeight(72)
	imageNudityEdit := qt.NewQTextEdit(nil)
	imageNudityEdit.SetPlaceholderText("e.g. completely nude, naked, no clothing")
	imageNudityEdit.SetPlainText(current.ImageNudity)
	imageNudityEdit.SetMinimumHeight(72)
	imageNegEdit := qt.NewQTextEdit(nil)
	imageNegEdit.SetPlaceholderText("things to keep out of the image")
	imageNegEdit.SetPlainText(current.ImageNegativePrompt)
	imageNegEdit.SetMinimumHeight(180)

	// Image test: hit ComfyUI's /system_stats and report the checkpoint
	// count. A full render would be the stronger test, but it's slow and
	// GPU-heavy — not something to fire from inside a settings dialog.
	imgTestRow, imgTestBtn, imgTestStatus := makeTestRow("Test connection")
	imgTestBtn.OnClicked(func() {
		imgTestBtn.SetEnabled(false)
		imgTestStatus.SetText("Testing…")
		qt.QCoreApplication_ProcessEvents()
		imgTestStatus.SetText(testComfyUIConnection(comfyEndpoint.Text()))
		imgTestBtn.SetEnabled(true)
	})

	// LLM tab: chat endpoint, credentials, model choice, prompt, budgets.
	llmForm, llmTab := newTab()
	llmForm.AddRow3("API endpoint:", endpoint.QWidget)
	llmForm.AddRow3("API key:", apiKey.QWidget)
	llmForm.AddRow3("Model:", model.QWidget)
	llmForm.AddRow4("System prompt:", systemPromptCol.QLayout)
	llmForm.AddRow3("Context length:", contextLabel.QWidget)
	llmForm.AddRow3("Message history:", historyMessages.QWidget)
	llmForm.AddRowWithLayout(llmTestRow.QLayout)

	// Speech-to-Text tab: the mic pipeline. Endpoint/key are optional and
	// fall back to the LLM ones at request time; the input device belongs
	// here because it's the source of the audio being transcribed.
	sttForm, sttTab := newTab()
	sttForm.AddRow3("Endpoint:", sttEndpoint.QWidget)
	sttForm.AddRow3("API key:", sttAPIKey.QWidget)
	sttForm.AddRow3("Model:", sttModel.QWidget)
	sttForm.AddRow3("Input device:", inputDevice.QWidget)
	sttForm.AddRowWithWidget(continuousConv.QWidget)
	sttForm.AddRowWithLayout(sttTestRow.QLayout)

	// Text-to-Speech tab: symmetric to STT, plus Voice and the toggle that
	// gates auto-speak. The output device sits here because it's where the
	// synthesized audio plays.
	ttsForm, ttsTab := newTab()
	ttsForm.AddRowWithWidget(enableTTS.QWidget)
	ttsForm.AddRow3("Endpoint:", ttsEndpoint.QWidget)
	ttsForm.AddRow3("API key:", ttsAPIKey.QWidget)
	ttsForm.AddRow3("Model:", ttsModel.QWidget)
	ttsForm.AddRow3("Voice:", ttsVoice.QWidget)
	ttsForm.AddRow3("Output device:", outputDevice.QWidget)
	ttsForm.AddRowWithLayout(ttsTestRow.QLayout)

	// Image Generation tab: the ComfyUI portrait pipeline. Enabled
	// independently of the other services since it talks to a separate
	// server; the prompt fields steer what Diesel looks like.
	imgForm, imgTab := newTab()
	imgForm.AddRowWithWidget(enableImageGen.QWidget)
	imgForm.AddRow3("ComfyUI endpoint:", comfyEndpoint.QWidget)
	imgForm.AddRow3("Image prompt:", imagePromptEdit.QWidget)
	imgForm.AddRow3("Clothing:", imageClothingEdit.QWidget)
	imgForm.AddRow3("Nudity:", imageNudityEdit.QWidget)
	imgForm.AddRow3("Negative prompt:", imageNegEdit.QWidget)
	imgForm.AddRowWithLayout(imgTestRow.QLayout)

	// Appearance tab: app-level UI preferences that don't belong to any one
	// service. Persisting conversations sits here because it's a global
	// behavior toggle rather than something tied to LLM/STT/TTS.
	apForm, apTab := newTab()
	apForm.AddRow3("Theme:", theme.QWidget)
	apForm.AddRowWithWidget(autoSave.QWidget)

	tabs := qt.NewQTabWidget(nil)
	tabs.AddTab(llmTab, "LLM")
	tabs.AddTab(sttTab, "Speech-to-Text")
	tabs.AddTab(ttsTab, "Text-to-Speech")
	tabs.AddTab(imgTab, "Image Generation")
	tabs.AddTab(apTab, "Appearance")
	root.AddWidget2(tabs.QWidget, 1)

	// Standard Save / Cancel pair. Qt swaps button order to match the
	// platform (macOS puts the affirmative button on the right).
	buttons := qt.NewQDialogButtonBox4(
		qt.QDialogButtonBox__Save | qt.QDialogButtonBox__Cancel,
	)
	buttons.OnAccepted(func() {
		updated := AppSettings{
			Theme:                  theme.CurrentText(),
			APIEndpoint:            endpoint.Text(),
			APIKey:                 apiKey.Text(),
			Model:                  model.CurrentText(),
			SystemPrompt:           systemPrompt.ToPlainText(),
			HistoryMessages:        historyMessages.Value(),
			STTEndpoint:            sttEndpoint.Text(),
			STTAPIKey:              sttAPIKey.Text(),
			STTModel:               sttModel.CurrentText(),
			ContinuousConversation: continuousConv.IsChecked(),
			EnableTTS:              enableTTS.IsChecked(),
			TTSEndpoint:            ttsEndpoint.Text(),
			TTSAPIKey:              ttsAPIKey.Text(),
			TTSModel:               ttsModel.CurrentText(),
			TTSVoice:               ttsVoice.Text(),
			InputDevice:            inputDevice.CurrentText(),
			OutputDevice:           outputDevice.CurrentText(),
			SaveToDisk:             autoSave.IsChecked(),
			EnableImageGen:         enableImageGen.IsChecked(),
			ComfyUIEndpoint:        comfyEndpoint.Text(),
			ImagePrompt:            imagePromptEdit.ToPlainText(),
			ImageClothing:          imageClothingEdit.ToPlainText(),
			ImageNudity:            imageNudityEdit.ToPlainText(),
			ImageNegativePrompt:    imageNegEdit.ToPlainText(),
		}
		if err := updated.save(); err != nil {
			qt.QMessageBox_Warning(parent, "Settings",
				"Could not save settings:\n"+err.Error())
			return
		}
		dlg.Accept()
	})
	buttons.OnRejected(func() { dlg.Reject() })
	root.AddWidget(buttons.QWidget)

	dlg.Exec()
}

// setComboSelection selects `value` in the combo if it's present, otherwise
// leaves the current index alone. Avoids a noisy "0 means default" pattern
// scattered through the call sites.
func setComboSelection(combo *qt.QComboBox, value string) {
	if value == "" {
		return
	}
	for i := 0; i < combo.Count(); i++ {
		if combo.ItemText(i) == value {
			combo.SetCurrentIndex(i)
			return
		}
	}
}

// populateModelCombo replaces `combo`'s items with the IDs `fetch` reports
// (sorted), then selects `saved` — adding it as an extra item when the
// server didn't list it, so an offline-at-dialog-time provider doesn't lose
// the user's prior choice. fetch errors are swallowed; the combo is still
// repopulated with at least the saved value.
func populateModelCombo(combo *qt.QComboBox, saved string, fetch func() ([]string, error)) {
	combo.Clear()
	if ids, err := fetch(); err == nil {
		sort.Strings(ids)
		for _, id := range ids {
			combo.AddItem(id)
		}
	}
	if saved == "" {
		return
	}
	setComboSelection(combo, saved)
	if combo.CurrentText() != saved {
		combo.AddItem(saved)
		setComboSelection(combo, saved)
	}
}
