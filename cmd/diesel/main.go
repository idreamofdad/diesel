package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"diesel/internal/audio"
	"diesel/internal/chat"
	"diesel/internal/comfyui"
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

	qt "github.com/mappu/miqt/qt6"
)

// Transcript speaker-label colors. Blue for both sides of the conversation;
// a soft red for inline error rows so they're easy to spot without being
// alarming against the dark background.
const (
	labelBlue = "#5aa7ff"
	labelRed  = "#e57373"
)

// desktopOrigin is the subscriber ID the Qt UI registers with the hub.
// Stable string (not a UUID) so the hub's "last-active wins" TTS
// routing and the busy-broadcast filter both know who "the desktop" is.
const desktopOrigin = "desktop"

// init pins the main goroutine to the process's main OS thread. macOS
// Cocoa requires every UI call — including the QApplication constructor
// and its menu setup — to run on that thread; without this the Go
// scheduler can migrate main() onto another thread (more likely now that
// startup opens the database first) and Qt aborts with an "API misuse:
// setting the main menu on a non-main thread" exception.
func init() {
	runtime.LockOSThread()
}

func main() {
	// -data-dir overrides where the database and character image live;
	// blank keeps the platform user-config default. Parsed first so it's
	// in effect before anything resolves a config path.
	dataDir := flag.String("data-dir", "", "directory for Diesel's data (database, character image); defaults to the OS user config dir")
	flag.Parse()
	if *dataDir != "" {
		util.SetConfigDir(*dataDir)
	}

	// OpenTelemetry: a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT (or the
	// trace-specific override) is set in the environment. Shutdown flushes
	// any in-flight spans on exit; bound to a 5 s deadline so a stuck
	// collector can't hang the app indefinitely.
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

	// SQLite-backed persistence: conversation history, the settings blob,
	// and each bridge's bookkeeping all live in one database file. Opened
	// before anything reads settings or history.
	dbPath, err := util.ConfigFilePath("diesel.db")
	if err != nil {
		log.Fatalf("[storage] config path: %v", err)
	}
	store, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("[storage] open: %v", err)
	}
	defer func() { _ = store.Close() }()
	// Wire the settings package to the database. settings can't import
	// storage (storage imports settings), so persistence is injected.
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
	// history is loaded by the time Qt is ready to paint it.
	h := hub.New(store)
	h.Start(context.Background())

	// HTTP server. Wired up before the window opens so it's reachable
	// immediately if EnableServer is on in the persisted settings.
	srvMgr := server.New(h, embeddedWebFS())
	srvMgr.Apply(settings.Load())
	defer srvMgr.Stop()

	// SMS bridge over Twilio. Same Apply/Stop shape as the HTTP server
	// — opt-in via settings, hot-reapplied on every Save.
	smsMgr := sms.New(h, store)
	smsMgr.Apply(settings.Load())
	defer smsMgr.Stop()

	// Telegram bot bridge. Same Apply/Stop shape — opt-in, hot-reapplied
	// on every Save.
	tgMgr := telegram.New(h, store)
	tgMgr.Apply(settings.Load())
	defer tgMgr.Stop()

	// Matrix bridge. Same Apply/Stop shape — opt-in, hot-reapplied on
	// every Save. Shares diesel.db with the rest of the persistence
	// layer; mautrix-go manages its own crypto/state tables there.
	mxMgr := matrix.New(h, store)
	mxMgr.Apply(settings.Load())
	defer mxMgr.Stop()

	qt.NewQApplication(os.Args)

	window := qt.NewQMainWindow(nil)
	window.SetWindowTitle("Diesel")
	window.Resize(960, 560)
	// Color palette matched to the Tk-rendered Python reference: dark grays
	// throughout, *gray* list selection (not the system blue), no harsh
	// borders.
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

	// Right column: the character portrait. The hub caches the most
	// recent portrait and the desktop subscriber pulls it via Portrait()
	// when EventTurnComplete arrives.
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
	portrait.SetSizePolicy2(qt.QSizePolicy__Fixed, qt.QSizePolicy__Preferred)
	portrait.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	portrait.SetToolTip("Double-click to view full size")
	portraitCol.AddWidget(portrait.QWidget)
	portraitCol.AddStretch()

	var latestPortraitPNG []byte
	// final=false is used for intermediate ComfyUI preview frames so the
	// double-click "view full size" handler keeps showing the last
	// fully-rendered PNG instead of a half-baked JPEG preview.
	showPortrait := func(data []byte, final bool) {
		if len(data) == 0 {
			return
		}
		pm := qt.NewQPixmap()
		if !pm.LoadFromDataWithData(data) || pm.IsNull() {
			return
		}
		if final {
			latestPortraitPNG = data
		}
		portrait.SetPixmap(pm.ScaledToWidth2(portraitWidth, qt.SmoothTransformation))
	}
	// Seed from the hub if it managed to load the cached portrait.
	if _, png := h.LatestPortrait(); len(png) > 0 {
		showPortrait(png, true)
	}

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

	convHdr := qt.NewQLabel5("Conversation", nil)
	hdrFont := convHdr.Font()
	hdrFont.SetBold(true)
	convHdr.SetFont(hdrFont)
	convCol.AddWidget(convHdr.QWidget)

	transcript := qt.NewQTextEdit(nil)
	transcript.SetReadOnly(true)
	transcript.SetPlaceholderText("(Conversation will appear here)")
	convCol.AddWidget(transcript.QWidget)

	// Paint any previously-persisted history that the hub loaded on Start.
	for _, m := range h.History() {
		switch m.Role {
		case chat.RoleUser:
			chat.AppendTurn(transcript, "You", m.Content, labelRed)
		case chat.RoleAssistant:
			chat.AppendTurn(transcript, "Diesel", m.Content, labelBlue)
		}
	}

	// Message input row.
	inputRow := qt.NewQHBoxLayout2()
	inputRow.SetSpacing(6)
	message := qt.NewQLineEdit(nil)
	message.SetPlaceholderText("Type a message...")
	sendBtn := qt.NewQPushButton3("Send")
	inputRow.AddWidget(message.QWidget)
	inputRow.AddWidget(sendBtn.QWidget)
	convCol.AddLayout(inputRow.QLayout)

	// Media controls row: record + upload buttons.
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
	footer := qt.NewQWidget(nil)
	footerLayout := qt.NewQHBoxLayout(footer)
	footerLayout.SetContentsMargins(14, 8, 14, 8)
	status := qt.NewQLabel5("Ready", nil)
	status.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	status.SetMinimumWidth(0)
	footerLayout.AddWidget2(status.QWidget, 1)

	tokensLabel := qt.NewQLabel5("", nil)
	tokensLabel.SetStyleSheet("color: #888; font-size: 11px;")
	footerLayout.AddWidget(tokensLabel.QWidget)

	outer.AddWidget(footer)

	statusRaw := "Ready"
	applyStatus := func() {
		w := status.Width()
		if w <= 0 {
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

	// ─── Hub subscription ─────────────────────────────────────────────────
	// The desktop UI is one of N possible clients now. The hub is the
	// canonical writer; this subscription is the only path through which
	// the transcript, portrait, status bar, and token counter are
	// updated. Events from the hub flow on a Go channel; a QTimer drains
	// them on the Qt main thread so widget mutations stay thread-safe.
	desktopSub := h.Subscribe(desktopOrigin)

	// Voice state has to live outside the hub — the hub doesn't care
	// where a turn came from, only the desktop subscriber does. We track
	// the most recent desktop send so the continuous-conversation loop
	// only re-arms the mic for voice-originated turns.
	var (
		lastDesktopWasVoice bool
		voice               *tts.Speaker
		rec                 *audio.Recorder
	)

	// startListening / sendDesktopMessage are mutually recursive (voice
	// → reply → listen again). Declared up front, assigned below.
	var startListening func()

	const (
		recordGlyphIdle   = "◉"
		recordGlyphActive = "⏹"
	)
	// Three orthogonal axes gate the text input + Send button:
	//   - recording: voice capture in progress
	//   - inFlight:  the hub is mid-turn (any origin)
	//   - identity:  FirstName / LastName / PetName all filled in settings
	// refreshInputEnabled collapses them into a single SetEnabled call so
	// every site that toggles one axis stays consistent. Identity is read
	// fresh each call (cheap; settings.Load is in-memory after first call).
	var (
		recording bool
		inFlight  bool
	)
	const identityHint = "Set first name, last name, and pet name in Settings → LLM."
	refreshInputEnabled := func() {
		identity := settings.Load().IdentityConfigured()
		enabled := !recording && !inFlight && identity
		message.SetEnabled(enabled)
		sendBtn.SetEnabled(enabled)
		if !identity {
			message.SetPlaceholderText(identityHint)
			message.SetToolTip(identityHint)
			sendBtn.SetToolTip(identityHint)
		} else {
			message.SetPlaceholderText("Type a message...")
			message.SetToolTip("")
			sendBtn.SetToolTip("")
		}
	}
	setRecordingUI := func(active bool) {
		recording = active
		if active {
			recordBtn.SetText(recordGlyphActive)
		} else {
			recordBtn.SetText(recordGlyphIdle)
		}
		uploadBtn.SetEnabled(true)
		refreshInputEnabled()
	}

	sendDesktopMessage := func(text string, viaVoice bool) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		message.Clear()
		lastDesktopWasVoice = viaVoice
		if err := h.Send(context.Background(), text, desktopOrigin, false); err != nil {
			setStatus("✗ " + err.Error())
		}
	}

	// dispatchEvent runs on the Qt main thread (called from the drain
	// timer). Every Qt-touching update for the desktop UI flows through
	// here. It's safe to call settings.Load(), tts.Play, comfyui state
	// readers, etc. from within.
	dispatchEvent := func(ev hub.Event) {
		switch ev.Type {
		case hub.EventTurnStarted:
			if ev.User != nil {
				chat.AppendTurn(transcript, "You", ev.User.Content, labelRed)
			}
			// Lock the UI regardless of origin — only one turn runs at
			// a time, so remote-initiated turns should also disable our
			// Send button to keep the user from racing.
			inFlight = true
			refreshInputEnabled()
		case hub.EventTurnComplete:
			// Text-only event now — audio and portrait arrive on
			// their own events as they finish. Re-enable input the
			// moment text is here so the next turn can start while
			// media is still rendering.
			if ev.Assistant != nil {
				chat.AppendTurn(transcript, "Diesel", ev.Assistant.Content, labelBlue)
			}
			inFlight = false
			refreshInputEnabled()
			message.SetFocus()
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
			// Last-active wins: only the originating subscriber plays.
			// Sentinel event with empty AudioURL = "no audio for this
			// turn" — fall straight through to the voice re-arm so
			// continuous-conversation mode doesn't hang.
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
					sp.OnDone = armNext
					voice = sp
					break
				}
			}
			armNext()
		case hub.EventPortraitProgress:
			// Intermediate preview frame from ComfyUI — paint it so the
			// user sees the image developing. Missing previews are fine
			// (cache evicted, slow event loop, etc.) — the next frame
			// or the final EventPortraitReady will land soon enough.
			if ev.PortraitURL != "" {
				id := strings.TrimPrefix(ev.PortraitURL, "/api/v1/portrait-preview/")
				if data, ok := h.PortraitPreview(id); ok {
					showPortrait(data, false)
				}
			}
		case hub.EventPortraitReady:
			if ev.PortraitURL != "" {
				id := strings.TrimPrefix(ev.PortraitURL, "/api/v1/portrait/")
				if data, ok := h.Portrait(id); ok {
					showPortrait(data, true)
				}
			}
		case hub.EventTurnError:
			if ev.Origin == desktopOrigin {
				chat.AppendTurn(transcript, "Error", ev.Error, labelRed)
				lastDesktopWasVoice = false
			}
			inFlight = false
			refreshInputEnabled()
		case hub.EventStatus:
			setStatus(ev.Status)
		case hub.EventCleared:
			transcript.Clear()
			tokensLabel.SetText("")
		case hub.EventBusy:
			setStatus("Busy — wait for the current reply to finish.")
		}
	}

	// Drain pump. 30 ms cadence matches the rest of the codebase's
	// PollAsync default and is fast enough that status updates feel
	// instant.
	drain := qt.NewQTimer()
	drain.SetSingleShot(false)
	drain.OnTimeout(func() {
		for {
			select {
			case ev, ok := <-desktopSub.Events:
				if !ok {
					drain.Stop()
					return
				}
				dispatchEvent(ev)
			default:
				return
			}
		}
	})
	drain.Start(30)

	sendBtn.OnClicked(func() { sendDesktopMessage(message.Text(), false) })
	message.OnReturnPressed(func() { sendDesktopMessage(message.Text(), false) })

	// ─── Record button + VAD ──────────────────────────────────────────────
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
			rec = nil
			setRecordingUI(false)
			if reason == audio.StopCancelled {
				setStatus("Ready")
				return
			}
			if reason == audio.StopNoSpeech {
				setStatus("No speech detected")
				return
			}
			setStatus("Transcribing…")
			s := settings.Load()
			ep := util.FirstNonEmpty(s.STTEndpoint, s.APIEndpoint)
			key := util.FirstNonEmpty(s.STTAPIKey, s.APIKey)
			wavBytes := audio.EncodeWAV(pcm)
			util.PollAsync(30,
				func() sttResult {
					t, err := audio.Transcribe(context.Background(), ep, key, s.STTModel, wavBytes)
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
					sendDesktopMessage(r.text, true)
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
			rec.Stop(audio.StopCancelled)
			return
		}
		startListening()
	})
	uploadBtn.OnClicked(func() {
		if rec != nil {
			rec.Stop(audio.StopCommitted)
		}
	})

	// ─── Menu bar ─────────────────────────────────────────────────────────
	mb := window.MenuBar()
	fileMenu := mb.AddMenuWithTitle("File")

	newAction := fileMenu.AddAction3(
		"New Conversation",
		qt.NewQKeySequence6(qt.QKeySequence__New),
	)
	newAction.OnTriggered(func() {
		if transcript.ToPlainText() == "" && len(h.History()) == 0 {
			return
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
		if err := h.Clear(context.Background()); err != nil {
			setStatus("✗ " + err.Error())
		}
	})

	prefsAction := qt.NewQAction2("Settings…")
	prefsAction.SetMenuRole(qt.QAction__PreferencesRole)
	prefsAction.OnTriggered(func() {
		showSettingsDialog(window.QWidget, srvMgr, smsMgr, tgMgr, mxMgr)
		// Identity is one of the gating axes for the Send affordance —
		// re-evaluate after the dialog closes so saving changes flips
		// the input from disabled to enabled (or back) immediately.
		refreshInputEnabled()
	})
	fileMenu.AddAction(prefsAction)

	// Initial input gate — if a fresh install lacks identity, the Send
	// button starts greyed with the configure-me hint.
	refreshInputEnabled()

	window.Show()
	qt.QApplication_Exec()

	// Tear down the hub subscription on quit. Stop the server first so
	// no in-flight WS handlers try to use a half-torn-down hub.
	srvMgr.Stop()
	smsMgr.Stop()
	tgMgr.Stop()
	mxMgr.Stop()
	h.Unsubscribe(desktopOrigin)
	h.Stop()
}

// showPortraitFullSize pops a modal viewer with the portrait scaled to
// fill the screen's available height. The dialog's width follows from
// the image's aspect ratio at that height.
func showPortraitFullSize(parent *qt.QWidget, png []byte) {
	pm := qt.NewQPixmap()
	if !pm.LoadFromDataWithData(png) || pm.IsNull() {
		return
	}

	w, h := pm.Width(), pm.Height()
	if screen := qt.QGuiApplication_PrimaryScreen(); screen != nil && h > 0 {
		avail := screen.AvailableGeometry()
		w = w * avail.Height() / h
		h = avail.Height()
		if w > avail.Width() {
			h = h * avail.Width() / w
			w = avail.Width()
		}
		pm = pm.Scaled3(w, h, qt.KeepAspectRatio, qt.SmoothTransformation)
		w, h = pm.Width(), pm.Height()
	}

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
// on-disk settings file. Save writes them back and applies any server
// or SMS config changes to the respective managers; Cancel discards.
func showSettingsDialog(parent *qt.QWidget, srvMgr *server.Manager, smsMgr *sms.Manager, tgMgr *telegram.Manager, mxMgr *matrix.Manager) {
	current := settings.Load()

	dlg := qt.NewQDialog(parent)
	dlg.SetWindowTitle("Settings")
	dlg.Resize(500, 600)

	root := qt.NewQVBoxLayout(dlg.QWidget)
	root.SetContentsMargins(18, 18, 18, 14)
	root.SetSpacing(12)

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

	const modelComboWidth = 280

	model := qt.NewQComboBox(nil)
	model.SetEditable(true)
	model.SetFixedWidth(modelComboWidth)
	loadModels := func(ep, key string) {
		populateModelCombo(model, current.Model, func() ([]string, error) {
			return settings.FetchModels(ep, key)
		})
	}
	loadModels(current.APIEndpoint, current.APIKey)

	// Audio devices.
	inputDevice := qt.NewQComboBox(nil)
	inputDevice.AddItem("System Default")
	for _, d := range audio.InputDescriptions() {
		inputDevice.AddItem(d)
	}
	setComboSelection(inputDevice, current.InputDevice)
	outputDevice := qt.NewQComboBox(nil)
	outputDevice.AddItem("System Default")
	for _, d := range audio.OutputDescriptions() {
		outputDevice.AddItem(d)
	}
	setComboSelection(outputDevice, current.OutputDevice)

	// STT settings.
	sttEndpoint := qt.NewQLineEdit3(current.STTEndpoint)
	sttEndpoint.SetPlaceholderText("(falls back to API endpoint)")
	sttAPIKey := qt.NewQLineEdit3(current.STTAPIKey)
	sttAPIKey.SetEchoMode(qt.QLineEdit__Password)
	sttAPIKey.SetPlaceholderText("(falls back to API key)")
	sttModel := qt.NewQComboBox(nil)
	sttModel.SetEditable(true)
	sttModel.SetFixedWidth(modelComboWidth)
	sttModel.LineEdit().SetPlaceholderText("whisper-1")
	loadSTTModels := func(ep, key string) {
		ep = util.FirstNonEmpty(ep, endpoint.Text())
		key = util.FirstNonEmpty(key, apiKey.Text())
		populateModelCombo(sttModel, current.STTModel, func() ([]string, error) {
			return settings.FetchSTTModels(ep, key)
		})
	}
	loadSTTModels(current.STTEndpoint, current.STTAPIKey)
	sttModelTimer := qt.NewQTimer()
	sttModelTimer.SetSingleShot(true)
	sttModelTimer.OnTimeout(func() {
		loadSTTModels(sttEndpoint.Text(), sttAPIKey.Text())
	})
	sttEndpoint.OnTextChanged(func(string) { sttModelTimer.Start(400) })
	sttAPIKey.OnTextChanged(func(string) { sttModelTimer.Start(400) })

	// TTS settings.
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
		ep = util.FirstNonEmpty(ep, endpoint.Text())
		key = util.FirstNonEmpty(key, apiKey.Text())
		populateModelCombo(ttsModel, current.TTSModel, func() ([]string, error) {
			return settings.FetchTTSModels(ep, key)
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

	// Identity. Three single-line inputs that get substituted into the
	// hardcoded persona prompt. Send is gated on all three being filled —
	// see refreshInputEnabled in the main window.
	firstName := qt.NewQLineEdit3(current.FirstName)
	firstName.SetPlaceholderText("First name")
	lastName := qt.NewQLineEdit3(current.LastName)
	lastName.SetPlaceholderText("Last name")
	petName := qt.NewQLineEdit3(current.PetName)
	petName.SetPlaceholderText("Pet name")

	// Context length.
	contextLabel := qt.NewQLabel5("—", nil)
	contextLabel.SetStyleSheet("color: #888;")
	refreshContext := func() {
		ep, key, mid := endpoint.Text(), apiKey.Text(), model.CurrentText()
		if strings.TrimSpace(ep) == "" || strings.TrimSpace(mid) == "" {
			contextLabel.SetText("—")
			return
		}
		contextLabel.SetText("Probing…")
		util.PollAsync(60, func() int {
			return settings.FetchModelContextLength(ep, key, mid)
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
	contextTimer.Start(0)

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

	// History length.
	historyMessages := qt.NewQSpinBox(nil)
	historyMessages.SetRange(0, 500)
	historyMessages.SetSingleStep(1)
	historyMessages.SetSuffix(" messages")
	historyMessages.SetValue(current.HistoryMessages)

	// Behavior.
	autoSave := qt.NewQCheckBox3("Save conversations to disk")
	autoSave.SetChecked(current.SaveToDisk)

	continuousConv := qt.NewQCheckBox3("Continuous conversation (keep listening after each reply)")
	continuousConv.SetChecked(current.ContinuousConversation)

	makeTestRow := func(label string) (*qt.QHBoxLayout, *qt.QPushButton, *qt.QLabel) {
		row := qt.NewQHBoxLayout2()
		btn := qt.NewQPushButton3(label)
		status := qt.NewQLabel5("", nil)
		status.SetWordWrap(true)
		row.AddWidget(btn.QWidget)
		row.AddWidget2(status.QWidget, 1)
		return row, btn, status
	}

	// LLM test.
	llmTestRow, llmTestBtn, llmTestStatus := makeTestRow("Test connection")
	llmTestBtn.OnClicked(func() {
		llmTestBtn.SetEnabled(false)
		llmTestStatus.SetText("Testing…")
		qt.QCoreApplication_ProcessEvents()
		result := settings.TestLLMConnection(endpoint.Text(), apiKey.Text())
		llmTestStatus.SetText(result)
		if strings.HasPrefix(result, "✓") {
			loadModels(endpoint.Text(), apiKey.Text())
			loadSTTModels(sttEndpoint.Text(), sttAPIKey.Text())
			loadTTSModels(ttsEndpoint.Text(), ttsAPIKey.Text())
			refreshContext()
		}
		llmTestBtn.SetEnabled(true)
	})

	// STT test.
	sttTestRow, sttTestBtn, sttTestStatus := makeTestRow("Test connection")
	sttTestBtn.OnClicked(func() {
		sttTestBtn.SetEnabled(false)
		sttTestStatus.SetText("Testing…")
		qt.QCoreApplication_ProcessEvents()
		ep := util.FirstNonEmpty(sttEndpoint.Text(), endpoint.Text())
		key := util.FirstNonEmpty(sttAPIKey.Text(), apiKey.Text())
		if strings.TrimSpace(ep) == "" {
			sttTestStatus.SetText("✗ No endpoint configured.")
		} else if ids, err := settings.FetchSTTModels(ep, key); err != nil {
			sttTestStatus.SetText("✗ " + err.Error())
		} else if len(ids) == 0 {
			sttTestStatus.SetText("✓ Connected, but the server returned no models.")
		} else {
			sttTestStatus.SetText(fmt.Sprintf("✓ Connected — %d model(s) available.", len(ids)))
			loadSTTModels(sttEndpoint.Text(), sttAPIKey.Text())
		}
		sttTestBtn.SetEnabled(true)
	})

	// TTS test.
	var testVoice *tts.Speaker
	ttsTestRow, ttsTestBtn, ttsTestStatus := makeTestRow("Test voice")
	ttsTestBtn.OnClicked(func() {
		ttsTestBtn.SetEnabled(false)
		ttsTestStatus.SetText("Synthesizing…")
		qt.QCoreApplication_ProcessEvents()
		if testVoice != nil {
			testVoice.Stop()
			testVoice = nil
		}
		ep := util.FirstNonEmpty(ttsEndpoint.Text(), endpoint.Text())
		key := util.FirstNonEmpty(ttsAPIKey.Text(), apiKey.Text())
		if strings.TrimSpace(ep) == "" {
			ttsTestStatus.SetText("✗ No endpoint configured.")
			ttsTestBtn.SetEnabled(true)
			return
		}
		audioBytes, err := tts.Synthesize(context.Background(), ep, key, ttsModel.CurrentText(), ttsVoice.Text(),
			"Testing, one two three.")
		if err != nil {
			ttsTestStatus.SetText("✗ " + err.Error())
			ttsTestBtn.SetEnabled(true)
			return
		}
		sp, err := tts.Play(context.Background(), audioBytes)
		if err != nil {
			ttsTestStatus.SetText("✗ " + err.Error())
			ttsTestBtn.SetEnabled(true)
			return
		}
		testVoice = sp
		ttsTestStatus.SetText("✓ Speaking…")
		ttsTestBtn.SetEnabled(true)
	})
	dlg.OnFinished(func(int) {
		if testVoice != nil {
			testVoice.Stop()
			testVoice = nil
		}
	})

	// Image generation.
	enableImageGen := qt.NewQCheckBox3("Render a character portrait after each reply")
	enableImageGen.SetChecked(current.EnableImageGen)
	comfyEndpoint := qt.NewQLineEdit3(current.ComfyUIEndpoint)
	comfyEndpoint.SetPlaceholderText("http://127.0.0.1:8188")

	imageSteps := qt.NewQSpinBox(nil)
	imageSteps.SetRange(1, 200)
	imageSteps.SetSingleStep(1)
	imageSteps.SetSuffix(" steps")
	imageSteps.SetValue(current.ImageSteps)

	imgTestRow, imgTestBtn, imgTestStatus := makeTestRow("Test connection")
	imgTestBtn.OnClicked(func() {
		imgTestBtn.SetEnabled(false)
		imgTestStatus.SetText("Testing…")
		qt.QCoreApplication_ProcessEvents()
		imgTestStatus.SetText(comfyui.TestConnection(comfyEndpoint.Text()))
		imgTestBtn.SetEnabled(true)
	})

	// ─── Server tab ──────────────────────────────────────────────────────
	// Mirror shape of the other service tabs: toggle, endpoint-ish config,
	// optional secret, status row. The status label reflects the live
	// state of srvMgr — refreshed on every Save and on dialog open so a
	// stale "Stopped" never persists past an apply.
	enableServer := qt.NewQCheckBox3("Enable HTTP server (remote web UI)")
	enableServer.SetChecked(current.EnableServer)
	serverExpose := qt.NewQCheckBox3("Expose to network (0.0.0.0) — otherwise loopback only")
	serverExpose.SetChecked(current.ServerExposeNetwork)
	serverPort := qt.NewQSpinBox(nil)
	serverPort.SetRange(1, 65535)
	serverPort.SetSingleStep(1)
	serverPort.SetValue(current.ServerPort)
	serverToken := qt.NewQLineEdit3(current.ServerAuthToken)
	serverToken.SetEchoMode(qt.QLineEdit__Password)
	serverToken.SetPlaceholderText("(blank = no auth — fine on loopback, risky on LAN)")
	serverStatus := qt.NewQLabel5(srvMgr.Status(), nil)
	serverStatus.SetWordWrap(true)
	serverStatus.SetStyleSheet("color: #aaa;")

	// ─── SMS tab ────────────────────────────────────────────────────────
	// Polled Twilio bridge — when on, the manager hits Twilio every
	// SMSPollSeconds and feeds inbound messages into the same hub the
	// desktop uses. Outbound replies are addressed to whichever number
	// most recently texted in. Auth Token is masked like the other
	// secret fields.
	enableSMS := qt.NewQCheckBox3("Enable SMS over Twilio (poll for messages)")
	enableSMS.SetChecked(current.EnableSMS)
	smsSID := qt.NewQLineEdit3(current.TwilioAccountSID)
	smsSID.SetPlaceholderText("ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	smsToken := qt.NewQLineEdit3(current.TwilioAuthToken)
	smsToken.SetEchoMode(qt.QLineEdit__Password)
	smsToken.SetPlaceholderText("(your Twilio Auth Token)")
	smsFrom := qt.NewQLineEdit3(current.TwilioFromNumber)
	smsFrom.SetPlaceholderText("+15551234567")
	smsAllowed := qt.NewQTextEdit(nil)
	smsAllowed.SetPlaceholderText("One number per line, e.g. +15551234567")
	smsAllowed.SetPlainText(strings.Join(current.SMSAllowedNumbers, "\n"))
	smsAllowed.SetMinimumHeight(96)
	smsPoll := qt.NewQSpinBox(nil)
	smsPoll.SetRange(3, 600)
	smsPoll.SetSingleStep(1)
	smsPoll.SetSuffix(" s")
	if current.SMSPollSeconds > 0 {
		smsPoll.SetValue(current.SMSPollSeconds)
	} else {
		smsPoll.SetValue(10)
	}
	smsStatus := qt.NewQLabel5(smsMgr.Status(), nil)
	smsStatus.SetWordWrap(true)
	smsStatus.SetStyleSheet("color: #aaa;")
	smsTestRow, smsTestBtn, smsTestStatus := makeTestRow("Test connection")
	smsTestBtn.OnClicked(func() {
		smsTestBtn.SetEnabled(false)
		smsTestStatus.SetText("Testing…")
		qt.QCoreApplication_ProcessEvents()
		c := &sms.Client{
			AccountSID: smsSID.Text(),
			AuthToken:  smsToken.Text(),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := c.Ping(ctx); err != nil {
			smsTestStatus.SetText("✗ " + err.Error())
		} else {
			smsTestStatus.SetText("✓ Connected to Twilio.")
		}
		smsTestBtn.SetEnabled(true)
	})

	// ─── Telegram tab ───────────────────────────────────────────────────
	// getUpdates long-poll bridge — when on, the manager feeds inbound
	// Telegram DMs into the same hub the desktop uses. Only the single
	// configured @username gets a reply. The bot token is masked like
	// the other secret fields. No poll-interval knob: long-poll has none.
	enableTelegram := qt.NewQCheckBox3("Enable Telegram bot (poll for messages)")
	enableTelegram.SetChecked(current.EnableTelegram)
	tgToken := qt.NewQLineEdit3(current.TelegramBotToken)
	tgToken.SetEchoMode(qt.QLineEdit__Password)
	tgToken.SetPlaceholderText("123456789:ABC… (from @BotFather)")
	tgUsername := qt.NewQLineEdit3(current.TelegramAllowedUsername)
	tgUsername.SetPlaceholderText("@username — the one allowed user")
	tgStatus := qt.NewQLabel5(tgMgr.Status(), nil)
	tgStatus.SetWordWrap(true)
	tgStatus.SetStyleSheet("color: #aaa;")
	tgTestRow, tgTestBtn, tgTestStatus := makeTestRow("Test connection")
	tgTestBtn.OnClicked(func() {
		tgTestBtn.SetEnabled(false)
		tgTestStatus.SetText("Testing…")
		qt.QCoreApplication_ProcessEvents()
		tgTestStatus.SetText(telegram.TestConnection(tgToken.Text()))
		tgTestBtn.SetEnabled(true)
	})

	// ─── Matrix tab ─────────────────────────────────────────────────────
	// /sync long-poll bridge with E2EE — when on, the bot logs in as
	// its configured MXID and operates in any room MatrixAllowedUser
	// invites it into, provided the room has exactly two members.
	// Password is masked. Homeserver URL is discovered from the bot
	// MXID's domain via .well-known, so the dialog doesn't expose it.
	enableMatrix := qt.NewQCheckBox3("Enable Matrix bot (E2EE sync)")
	enableMatrix.SetChecked(current.EnableMatrix)
	mxBotID := qt.NewQLineEdit3(current.MatrixBotUserID)
	mxBotID.SetPlaceholderText("@diesel:matrix.org — the bot's own MXID")
	mxPassword := qt.NewQLineEdit3(current.MatrixPassword)
	mxPassword.SetEchoMode(qt.QLineEdit__Password)
	mxPassword.SetPlaceholderText("(password for the bot account)")
	mxAllowed := qt.NewQLineEdit3(current.MatrixAllowedUser)
	mxAllowed.SetPlaceholderText("@you:matrix.org — the one allowed user")
	mxStatus := qt.NewQLabel5(mxMgr.Status(), nil)
	mxStatus.SetWordWrap(true)
	mxStatus.SetStyleSheet("color: #aaa;")
	mxTestRow, mxTestBtn, mxTestStatus := makeTestRow("Test connection")
	mxTestBtn.OnClicked(func() {
		mxTestBtn.SetEnabled(false)
		mxTestStatus.SetText("Testing…")
		qt.QCoreApplication_ProcessEvents()
		mxTestStatus.SetText(matrix.TestConnection(mxBotID.Text(), mxPassword.Text()))
		mxTestBtn.SetEnabled(true)
	})

	// LLM tab.
	llmForm, llmTab := newTab()
	llmForm.AddRow3("API endpoint:", endpoint.QWidget)
	llmForm.AddRow3("API key:", apiKey.QWidget)
	llmForm.AddRow3("Model:", model.QWidget)
	llmForm.AddRow3("First name:", firstName.QWidget)
	llmForm.AddRow3("Last name:", lastName.QWidget)
	llmForm.AddRow3("Pet name:", petName.QWidget)
	llmForm.AddRow3("Context length:", contextLabel.QWidget)
	llmForm.AddRow3("Message history:", historyMessages.QWidget)
	llmForm.AddRowWithLayout(llmTestRow.QLayout)

	// STT tab.
	sttForm, sttTab := newTab()
	sttForm.AddRow3("Endpoint:", sttEndpoint.QWidget)
	sttForm.AddRow3("API key:", sttAPIKey.QWidget)
	sttForm.AddRow3("Model:", sttModel.QWidget)
	sttForm.AddRow3("Input device:", inputDevice.QWidget)
	sttForm.AddRowWithWidget(continuousConv.QWidget)
	sttForm.AddRowWithLayout(sttTestRow.QLayout)

	// TTS tab.
	ttsForm, ttsTab := newTab()
	ttsForm.AddRowWithWidget(enableTTS.QWidget)
	ttsForm.AddRow3("Endpoint:", ttsEndpoint.QWidget)
	ttsForm.AddRow3("API key:", ttsAPIKey.QWidget)
	ttsForm.AddRow3("Model:", ttsModel.QWidget)
	ttsForm.AddRow3("Voice:", ttsVoice.QWidget)
	ttsForm.AddRow3("Output device:", outputDevice.QWidget)
	ttsForm.AddRowWithLayout(ttsTestRow.QLayout)

	// Image tab.
	imgForm, imgTab := newTab()
	imgForm.AddRowWithWidget(enableImageGen.QWidget)
	imgForm.AddRow3("ComfyUI endpoint:", comfyEndpoint.QWidget)
	imgForm.AddRow3("Steps:", imageSteps.QWidget)
	imgForm.AddRowWithLayout(imgTestRow.QLayout)

	// Server tab.
	srvForm, srvTab := newTab()
	srvForm.AddRowWithWidget(enableServer.QWidget)
	srvForm.AddRowWithWidget(serverExpose.QWidget)
	srvForm.AddRow3("Port:", serverPort.QWidget)
	srvForm.AddRow3("Auth token:", serverToken.QWidget)
	srvForm.AddRow3("Status:", serverStatus.QWidget)

	// SMS tab.
	smsForm, smsTab := newTab()
	smsForm.AddRowWithWidget(enableSMS.QWidget)
	smsForm.AddRow3("Account SID:", smsSID.QWidget)
	smsForm.AddRow3("Auth Token:", smsToken.QWidget)
	smsForm.AddRow3("From number:", smsFrom.QWidget)
	smsForm.AddRow3("Allowed numbers:", smsAllowed.QWidget)
	smsForm.AddRow3("Poll interval:", smsPoll.QWidget)
	smsForm.AddRow3("Status:", smsStatus.QWidget)
	smsForm.AddRowWithLayout(smsTestRow.QLayout)

	// Telegram tab.
	tgForm, tgTab := newTab()
	tgForm.AddRowWithWidget(enableTelegram.QWidget)
	tgForm.AddRow3("Bot token:", tgToken.QWidget)
	tgForm.AddRow3("Allowed username:", tgUsername.QWidget)
	tgForm.AddRow3("Status:", tgStatus.QWidget)
	tgForm.AddRowWithLayout(tgTestRow.QLayout)

	// Matrix tab.
	mxForm, mxTab := newTab()
	mxForm.AddRowWithWidget(enableMatrix.QWidget)
	mxForm.AddRow3("Bot user ID:", mxBotID.QWidget)
	mxForm.AddRow3("Password:", mxPassword.QWidget)
	mxForm.AddRow3("Allowed user:", mxAllowed.QWidget)
	mxForm.AddRow3("Status:", mxStatus.QWidget)
	mxForm.AddRowWithLayout(mxTestRow.QLayout)

	// Appearance.
	apForm, apTab := newTab()
	apForm.AddRow3("Theme:", theme.QWidget)
	apForm.AddRowWithWidget(autoSave.QWidget)

	tabs := qt.NewQTabWidget(nil)
	tabs.AddTab(llmTab, "LLM")
	tabs.AddTab(sttTab, "Speech-to-Text")
	tabs.AddTab(ttsTab, "Text-to-Speech")
	tabs.AddTab(imgTab, "Image Generation")
	tabs.AddTab(srvTab, "Server")
	tabs.AddTab(smsTab, "SMS")
	tabs.AddTab(tgTab, "Telegram")
	tabs.AddTab(mxTab, "Matrix")
	tabs.AddTab(apTab, "Appearance")
	root.AddWidget2(tabs.QWidget, 1)

	buttons := qt.NewQDialogButtonBox4(
		qt.QDialogButtonBox__Save | qt.QDialogButtonBox__Cancel,
	)
	buttons.OnAccepted(func() {
		// Split the allowed-numbers textarea into a clean []string —
		// trim each line and drop blanks so a trailing newline doesn't
		// turn into an empty entry the manager would have to filter.
		var allowed []string
		for _, line := range strings.Split(smsAllowed.ToPlainText(), "\n") {
			if v := strings.TrimSpace(line); v != "" {
				allowed = append(allowed, v)
			}
		}
		updated := settings.AppSettings{
			Theme:                   theme.CurrentText(),
			APIEndpoint:             endpoint.Text(),
			APIKey:                  apiKey.Text(),
			Model:                   model.CurrentText(),
			FirstName:               firstName.Text(),
			LastName:                lastName.Text(),
			PetName:                 petName.Text(),
			HistoryMessages:         historyMessages.Value(),
			STTEndpoint:             sttEndpoint.Text(),
			STTAPIKey:               sttAPIKey.Text(),
			STTModel:                sttModel.CurrentText(),
			ContinuousConversation:  continuousConv.IsChecked(),
			EnableTTS:               enableTTS.IsChecked(),
			TTSEndpoint:             ttsEndpoint.Text(),
			TTSAPIKey:               ttsAPIKey.Text(),
			TTSModel:                ttsModel.CurrentText(),
			TTSVoice:                ttsVoice.Text(),
			InputDevice:             inputDevice.CurrentText(),
			OutputDevice:            outputDevice.CurrentText(),
			SaveToDisk:              autoSave.IsChecked(),
			EnableImageGen:          enableImageGen.IsChecked(),
			ComfyUIEndpoint:         comfyEndpoint.Text(),
			ImageSteps:              imageSteps.Value(),
			EnableServer:            enableServer.IsChecked(),
			ServerExposeNetwork:     serverExpose.IsChecked(),
			ServerPort:              serverPort.Value(),
			ServerAuthToken:         serverToken.Text(),
			EnableSMS:               enableSMS.IsChecked(),
			TwilioAccountSID:        smsSID.Text(),
			TwilioAuthToken:         smsToken.Text(),
			TwilioFromNumber:        smsFrom.Text(),
			SMSAllowedNumbers:       allowed,
			SMSPollSeconds:          smsPoll.Value(),
			EnableTelegram:          enableTelegram.IsChecked(),
			TelegramBotToken:        tgToken.Text(),
			TelegramAllowedUsername: strings.TrimSpace(tgUsername.Text()),
			EnableMatrix:            enableMatrix.IsChecked(),
			MatrixBotUserID:         strings.TrimSpace(mxBotID.Text()),
			MatrixPassword:          mxPassword.Text(),
			MatrixAllowedUser:       strings.TrimSpace(mxAllowed.Text()),
		}
		if err := updated.Save(); err != nil {
			qt.QMessageBox_Warning(parent, "Settings",
				"Could not save settings:\n"+err.Error())
			return
		}
		// Hot-restart the server against the new config. The status
		// label updates so the user sees a failed bind without closing
		// the dialog.
		serverStatus.SetText(srvMgr.Apply(updated))
		// Apply the SMS, Telegram, and Matrix configs to their managers
		// too — same shape, same "keep dialog open on failure"
		// semantics. Matrix's Apply may take a moment because login
		// happens on a background goroutine, but it returns the
		// "Connecting…" status immediately so the dialog never blocks.
		smsStatus.SetText(smsMgr.Apply(updated))
		tgStatus.SetText(tgMgr.Apply(updated))
		mxStatus.SetText(mxMgr.Apply(updated))
		// If any apply failed, give the user a chance to fix it instead
		// of closing the dialog out from under them.
		if strings.HasPrefix(serverStatus.Text(), "✗") ||
			strings.HasPrefix(smsStatus.Text(), "✗") ||
			strings.HasPrefix(tgStatus.Text(), "✗") ||
			strings.HasPrefix(mxStatus.Text(), "✗") {
			return
		}
		dlg.Accept()
	})
	buttons.OnRejected(func() { dlg.Reject() })
	root.AddWidget(buttons.QWidget)

	dlg.Exec()
}

// setComboSelection selects `value` in the combo if it's present.
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

// populateModelCombo replaces `combo`'s items with the IDs `fetch`
// reports (sorted), then selects `saved`.
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
