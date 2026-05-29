package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"diesel/internal/audio"
	"diesel/internal/comfyui"
	"diesel/internal/matrix"
	"diesel/internal/server"
	"diesel/internal/settings"
	"diesel/internal/sms"
	"diesel/internal/telegram"
	"diesel/internal/tts"
	"diesel/internal/util"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// newIntEntry returns an entry constrained to whole numbers in [min,max].
// It replaces Qt's QSpinBox, which Fyne has no stable equivalent for.
func newIntEntry(val, min, max int) *widget.Entry {
	e := widget.NewEntry()
	e.SetText(strconv.Itoa(val))
	e.Validator = func(s string) error {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return errors.New("must be a whole number")
		}
		if n < min || n > max {
			return fmt.Errorf("must be between %d and %d", min, max)
		}
		return nil
	}
	return e
}

// intOr parses the entry's text, falling back to def on anything invalid so a
// half-typed value can't wipe a setting on save.
func intOr(e *widget.Entry, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(e.Text)); err == nil {
		return n
	}
	return def
}

// debounce returns a function that runs fn only once activity stops for d.
// fn is marshalled onto the UI goroutine, so callers can touch widgets in it.
func debounce(d time.Duration, fn func()) func() {
	var timer *time.Timer
	return func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(d, func() { fyne.Do(fn) })
	}
}

// populateModelSelect fetches model IDs off the UI thread and fills the
// editable combo, keeping the saved selection present even if the server
// doesn't list it. Mirrors the Qt populateModelCombo.
func populateModelSelect(sel *widget.SelectEntry, saved string, fetch func() ([]string, error)) {
	uiAsync(func() []string {
		ids, err := fetch()
		if err != nil {
			return nil
		}
		sort.Strings(ids)
		return ids
	}, func(ids []string) {
		if saved != "" {
			present := false
			for _, id := range ids {
				if id == saved {
					present = true
					break
				}
			}
			if !present {
				ids = append(ids, saved)
			}
		}
		sel.SetOptions(ids)
		if saved != "" {
			sel.SetText(saved)
		}
	})
}

// makeTestRow builds a "[button] status…" row used by every service tab.
func makeTestRow(label string) (fyne.CanvasObject, *widget.Button, *widget.Label) {
	status := widget.NewLabel("")
	status.Wrapping = fyne.TextWrapWord
	btn := widget.NewButton(label, nil)
	return container.NewBorder(nil, nil, btn, nil, status), btn, status
}

// showSettingsDialog presents a modal settings dialog populated from the
// persisted settings. Save writes them back and re-applies the server/SMS/
// Telegram/Matrix configs to their managers; Cancel discards. onClosed runs
// after the dialog closes either way (the caller re-evaluates the Send gate).
func showSettingsDialog(win fyne.Window, srvMgr *server.Manager, smsMgr *sms.Manager, tgMgr *telegram.Manager, mxMgr *matrix.Manager, onClosed func()) {
	current := settings.Load()

	// ─── LLM ───────────────────────────────────────────────────────────────
	endpoint := widget.NewEntry()
	endpoint.SetText(current.APIEndpoint)
	apiKey := widget.NewPasswordEntry()
	apiKey.SetText(current.APIKey)
	apiKey.SetPlaceHolder("sk-…")
	model := widget.NewSelectEntry([]string{})

	firstName := widget.NewEntry()
	firstName.SetText(current.FirstName)
	firstName.SetPlaceHolder("First name")
	lastName := widget.NewEntry()
	lastName.SetText(current.LastName)
	lastName.SetPlaceHolder("Last name")
	petName := widget.NewEntry()
	petName.SetText(current.PetName)
	petName.SetPlaceHolder("Pet name")

	contextLabel := widget.NewLabel("—")
	historyMessages := newIntEntry(current.HistoryMessages, 0, 500)

	// ─── STT ───────────────────────────────────────────────────────────────
	sttEndpoint := widget.NewEntry()
	sttEndpoint.SetText(current.STTEndpoint)
	sttEndpoint.SetPlaceHolder("(falls back to API endpoint)")
	sttAPIKey := widget.NewPasswordEntry()
	sttAPIKey.SetText(current.STTAPIKey)
	sttAPIKey.SetPlaceHolder("(falls back to API key)")
	sttModel := widget.NewSelectEntry([]string{})
	sttModel.SetPlaceHolder("whisper-1")
	inputDevice := widget.NewSelect(append([]string{"System Default"}, audio.InputDescriptions()...), nil)
	inputDevice.SetSelected(current.InputDevice)
	if inputDevice.Selected == "" {
		inputDevice.SetSelected("System Default")
	}
	continuousConv := widget.NewCheck("Continuous conversation (keep listening after each reply)", nil)
	continuousConv.SetChecked(current.ContinuousConversation)

	// ─── TTS ───────────────────────────────────────────────────────────────
	enableTTS := widget.NewCheck("Speak replies through TTS", nil)
	enableTTS.SetChecked(current.EnableTTS)
	ttsEndpoint := widget.NewEntry()
	ttsEndpoint.SetText(current.TTSEndpoint)
	ttsEndpoint.SetPlaceHolder("(falls back to API endpoint)")
	ttsAPIKey := widget.NewPasswordEntry()
	ttsAPIKey.SetText(current.TTSAPIKey)
	ttsAPIKey.SetPlaceHolder("(falls back to API key)")
	ttsModel := widget.NewSelectEntry([]string{})
	ttsModel.SetPlaceHolder("tts-1")
	ttsVoice := widget.NewEntry()
	ttsVoice.SetText(current.TTSVoice)
	ttsVoice.SetPlaceHolder("alloy")
	outputDevice := widget.NewSelect(append([]string{"System Default"}, audio.OutputDescriptions()...), nil)
	outputDevice.SetSelected(current.OutputDevice)
	if outputDevice.Selected == "" {
		outputDevice.SetSelected("System Default")
	}

	// ─── Image generation ──────────────────────────────────────────────────
	enableImageGen := widget.NewCheck("Render a character portrait after each reply", nil)
	enableImageGen.SetChecked(current.EnableImageGen)
	comfyEndpoint := widget.NewEntry()
	comfyEndpoint.SetText(current.ComfyUIEndpoint)
	comfyEndpoint.SetPlaceHolder("http://127.0.0.1:8188")
	imageSteps := newIntEntry(current.ImageSteps, 1, 200)

	// ─── Server ────────────────────────────────────────────────────────────
	enableServer := widget.NewCheck("Enable HTTP server (remote web UI)", nil)
	enableServer.SetChecked(current.EnableServer)
	serverExpose := widget.NewCheck("Expose to network (0.0.0.0) — otherwise loopback only", nil)
	serverExpose.SetChecked(current.ServerExposeNetwork)
	serverPort := newIntEntry(current.ServerPort, 1, 65535)
	serverToken := widget.NewPasswordEntry()
	serverToken.SetText(current.ServerAuthToken)
	serverToken.SetPlaceHolder("(blank = no auth — fine on loopback, risky on LAN)")
	serverStatus := widget.NewLabel(srvMgr.Status())
	serverStatus.Wrapping = fyne.TextWrapWord

	// ─── SMS ───────────────────────────────────────────────────────────────
	enableSMS := widget.NewCheck("Enable SMS over Twilio (poll for messages)", nil)
	enableSMS.SetChecked(current.EnableSMS)
	smsSID := widget.NewEntry()
	smsSID.SetText(current.TwilioAccountSID)
	smsSID.SetPlaceHolder("ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	smsToken := widget.NewPasswordEntry()
	smsToken.SetText(current.TwilioAuthToken)
	smsToken.SetPlaceHolder("(your Twilio Auth Token)")
	smsFrom := widget.NewEntry()
	smsFrom.SetText(current.TwilioFromNumber)
	smsFrom.SetPlaceHolder("+15551234567")
	smsAllowed := widget.NewMultiLineEntry()
	smsAllowed.SetText(strings.Join(current.SMSAllowedNumbers, "\n"))
	smsAllowed.SetPlaceHolder("One number per line, e.g. +15551234567")
	smsPollDefault := current.SMSPollSeconds
	if smsPollDefault <= 0 {
		smsPollDefault = 10
	}
	smsPoll := newIntEntry(smsPollDefault, 3, 600)
	smsStatus := widget.NewLabel(smsMgr.Status())
	smsStatus.Wrapping = fyne.TextWrapWord

	// ─── Telegram ──────────────────────────────────────────────────────────
	enableTelegram := widget.NewCheck("Enable Telegram bot (poll for messages)", nil)
	enableTelegram.SetChecked(current.EnableTelegram)
	tgToken := widget.NewPasswordEntry()
	tgToken.SetText(current.TelegramBotToken)
	tgToken.SetPlaceHolder("123456789:ABC… (from @BotFather)")
	tgUsername := widget.NewEntry()
	tgUsername.SetText(current.TelegramAllowedUsername)
	tgUsername.SetPlaceHolder("@username — the one allowed user")
	tgStatus := widget.NewLabel(tgMgr.Status())
	tgStatus.Wrapping = fyne.TextWrapWord

	// ─── Matrix ────────────────────────────────────────────────────────────
	enableMatrix := widget.NewCheck("Enable Matrix bot (E2EE sync)", nil)
	enableMatrix.SetChecked(current.EnableMatrix)
	mxBotID := widget.NewEntry()
	mxBotID.SetText(current.MatrixBotUserID)
	mxBotID.SetPlaceHolder("@diesel:matrix.org — the bot's own MXID")
	mxPassword := widget.NewPasswordEntry()
	mxPassword.SetText(current.MatrixPassword)
	mxPassword.SetPlaceHolder("(password for the bot account)")
	mxAllowed := widget.NewEntry()
	mxAllowed.SetText(current.MatrixAllowedUser)
	mxAllowed.SetPlaceHolder("@you:matrix.org — the one allowed user")
	mxStatus := widget.NewLabel(mxMgr.Status())
	mxStatus.Wrapping = fyne.TextWrapWord

	// ─── Appearance ────────────────────────────────────────────────────────
	themeSel := widget.NewSelect([]string{"System", "Dark", "Light"}, nil)
	themeSel.SetSelected(current.Theme)
	if themeSel.Selected == "" {
		themeSel.SetSelected("System")
	}
	autoSave := widget.NewCheck("Save conversations to disk", nil)
	autoSave.SetChecked(current.SaveToDisk)

	// ─── Model-list loaders + context probe ────────────────────────────────
	// Each reload preserves the originally-saved selection (current.X), the
	// same shape the Qt build used. STT/TTS endpoints fall back to the LLM
	// endpoint/key when blank.
	loadModels := func() {
		populateModelSelect(model, current.Model, func() ([]string, error) {
			return settings.FetchModels(endpoint.Text, apiKey.Text)
		})
	}
	loadSTTModels := func() {
		ep := util.FirstNonEmpty(sttEndpoint.Text, endpoint.Text)
		key := util.FirstNonEmpty(sttAPIKey.Text, apiKey.Text)
		populateModelSelect(sttModel, current.STTModel, func() ([]string, error) {
			return settings.FetchSTTModels(ep, key)
		})
	}
	loadTTSModels := func() {
		ep := util.FirstNonEmpty(ttsEndpoint.Text, endpoint.Text)
		key := util.FirstNonEmpty(ttsAPIKey.Text, apiKey.Text)
		populateModelSelect(ttsModel, current.TTSModel, func() ([]string, error) {
			return settings.FetchTTSModels(ep, key)
		})
	}
	refreshContext := func() {
		ep, key, mid := endpoint.Text, apiKey.Text, model.Text
		if strings.TrimSpace(ep) == "" || strings.TrimSpace(mid) == "" {
			contextLabel.SetText("—")
			return
		}
		contextLabel.SetText("Probing…")
		uiAsync(func() int {
			return settings.FetchModelContextLength(ep, key, mid)
		}, func(n int) {
			if n <= 0 {
				contextLabel.SetText("not reported by this server")
				return
			}
			contextLabel.SetText(fmt.Sprintf("%d tokens", n))
		})
	}

	// Debounced live refreshes as the user types — same 400 ms cadence and
	// cross-field fallback rules as the Qt dialog.
	dCtx := debounce(400*time.Millisecond, refreshContext)
	dSTT := debounce(400*time.Millisecond, loadSTTModels)
	dTTS := debounce(400*time.Millisecond, loadTTSModels)
	model.OnChanged = func(string) { dCtx() }
	sttEndpoint.OnChanged = func(string) { dSTT() }
	sttAPIKey.OnChanged = func(string) { dSTT() }
	ttsEndpoint.OnChanged = func(string) { dTTS() }
	ttsAPIKey.OnChanged = func(string) { dTTS() }
	endpoint.OnChanged = func(string) {
		if strings.TrimSpace(sttEndpoint.Text) == "" {
			dSTT()
		}
		if strings.TrimSpace(ttsEndpoint.Text) == "" {
			dTTS()
		}
		dCtx()
	}
	apiKey.OnChanged = func(string) {
		if strings.TrimSpace(sttAPIKey.Text) == "" {
			dSTT()
		}
		if strings.TrimSpace(ttsAPIKey.Text) == "" {
			dTTS()
		}
		dCtx()
	}

	// ─── Test rows ─────────────────────────────────────────────────────────
	llmTestRow, llmTestBtn, llmTestStatus := makeTestRow("Test connection")
	llmTestBtn.OnTapped = func() {
		llmTestBtn.Disable()
		llmTestStatus.SetText("Testing…")
		ep, key := endpoint.Text, apiKey.Text
		uiAsync(func() string { return settings.TestLLMConnection(ep, key) }, func(result string) {
			llmTestStatus.SetText(result)
			if strings.HasPrefix(result, "✓") {
				loadModels()
				loadSTTModels()
				loadTTSModels()
				refreshContext()
			}
			llmTestBtn.Enable()
		})
	}

	sttTestRow, sttTestBtn, sttTestStatus := makeTestRow("Test connection")
	sttTestBtn.OnTapped = func() {
		ep := util.FirstNonEmpty(sttEndpoint.Text, endpoint.Text)
		key := util.FirstNonEmpty(sttAPIKey.Text, apiKey.Text)
		if strings.TrimSpace(ep) == "" {
			sttTestStatus.SetText("✗ No endpoint configured.")
			return
		}
		sttTestBtn.Disable()
		sttTestStatus.SetText("Testing…")
		type res struct {
			ids []string
			err error
		}
		uiAsync(func() res {
			ids, err := settings.FetchSTTModels(ep, key)
			return res{ids, err}
		}, func(r res) {
			switch {
			case r.err != nil:
				sttTestStatus.SetText("✗ " + r.err.Error())
			case len(r.ids) == 0:
				sttTestStatus.SetText("✓ Connected, but the server returned no models.")
			default:
				sttTestStatus.SetText(fmt.Sprintf("✓ Connected — %d model(s) available.", len(r.ids)))
				loadSTTModels()
			}
			sttTestBtn.Enable()
		})
	}

	// TTS test synthesizes a short phrase and plays it. testVoice is stopped
	// when the dialog closes so a long sample can't outlive the dialog.
	var testVoice *tts.Speaker
	ttsTestRow, ttsTestBtn, ttsTestStatus := makeTestRow("Test voice")
	ttsTestBtn.OnTapped = func() {
		ep := util.FirstNonEmpty(ttsEndpoint.Text, endpoint.Text)
		key := util.FirstNonEmpty(ttsAPIKey.Text, apiKey.Text)
		if strings.TrimSpace(ep) == "" {
			ttsTestStatus.SetText("✗ No endpoint configured.")
			return
		}
		if testVoice != nil {
			testVoice.Stop()
			testVoice = nil
		}
		ttsTestBtn.Disable()
		ttsTestStatus.SetText("Synthesizing…")
		mdl, voice := ttsModel.Text, ttsVoice.Text
		type res struct {
			audio []byte
			err   error
		}
		uiAsync(func() res {
			ab, err := tts.Synthesize(context.Background(), ep, key, mdl, voice, "Testing, one two three.")
			return res{ab, err}
		}, func(r res) {
			defer ttsTestBtn.Enable()
			if r.err != nil {
				ttsTestStatus.SetText("✗ " + r.err.Error())
				return
			}
			sp, err := tts.Play(context.Background(), r.audio)
			if err != nil {
				ttsTestStatus.SetText("✗ " + err.Error())
				return
			}
			testVoice = sp
			ttsTestStatus.SetText("✓ Speaking…")
		})
	}

	imgTestRow, imgTestBtn, imgTestStatus := makeTestRow("Test connection")
	imgTestBtn.OnTapped = func() {
		imgTestBtn.Disable()
		imgTestStatus.SetText("Testing…")
		ep := comfyEndpoint.Text
		uiAsync(func() string { return comfyui.TestConnection(ep) }, func(result string) {
			imgTestStatus.SetText(result)
			imgTestBtn.Enable()
		})
	}

	smsTestRow, smsTestBtn, smsTestStatus := makeTestRow("Test connection")
	smsTestBtn.OnTapped = func() {
		smsTestBtn.Disable()
		smsTestStatus.SetText("Testing…")
		c := &sms.Client{AccountSID: smsSID.Text, AuthToken: smsToken.Text}
		uiAsync(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			return c.Ping(ctx)
		}, func(err error) {
			if err != nil {
				smsTestStatus.SetText("✗ " + err.Error())
			} else {
				smsTestStatus.SetText("✓ Connected to Twilio.")
			}
			smsTestBtn.Enable()
		})
	}

	tgTestRow, tgTestBtn, tgTestStatus := makeTestRow("Test connection")
	tgTestBtn.OnTapped = func() {
		tgTestBtn.Disable()
		tgTestStatus.SetText("Testing…")
		token := tgToken.Text
		uiAsync(func() string { return telegram.TestConnection(token) }, func(result string) {
			tgTestStatus.SetText(result)
			tgTestBtn.Enable()
		})
	}

	mxTestRow, mxTestBtn, mxTestStatus := makeTestRow("Test connection")
	mxTestBtn.OnTapped = func() {
		mxTestBtn.Disable()
		mxTestStatus.SetText("Testing…")
		botID, pw := mxBotID.Text, mxPassword.Text
		uiAsync(func() string { return matrix.TestConnection(botID, pw) }, func(result string) {
			mxTestStatus.SetText(result)
			mxTestBtn.Enable()
		})
	}

	// ─── Tabs ──────────────────────────────────────────────────────────────
	tab := func(items ...*widget.FormItem) fyne.CanvasObject {
		return container.NewVScroll(widget.NewForm(items...))
	}
	full := func(w fyne.CanvasObject) *widget.FormItem { return widget.NewFormItem("", w) }

	llmTab := tab(
		widget.NewFormItem("API endpoint", endpoint),
		widget.NewFormItem("API key", apiKey),
		widget.NewFormItem("Model", model),
		widget.NewFormItem("First name", firstName),
		widget.NewFormItem("Last name", lastName),
		widget.NewFormItem("Pet name", petName),
		widget.NewFormItem("Context length", contextLabel),
		widget.NewFormItem("Message history", historyMessages),
		full(llmTestRow),
	)
	sttTab := tab(
		widget.NewFormItem("Endpoint", sttEndpoint),
		widget.NewFormItem("API key", sttAPIKey),
		widget.NewFormItem("Model", sttModel),
		widget.NewFormItem("Input device", inputDevice),
		full(continuousConv),
		full(sttTestRow),
	)
	ttsTab := tab(
		full(enableTTS),
		widget.NewFormItem("Endpoint", ttsEndpoint),
		widget.NewFormItem("API key", ttsAPIKey),
		widget.NewFormItem("Model", ttsModel),
		widget.NewFormItem("Voice", ttsVoice),
		widget.NewFormItem("Output device", outputDevice),
		full(ttsTestRow),
	)
	imgTab := tab(
		full(enableImageGen),
		widget.NewFormItem("ComfyUI endpoint", comfyEndpoint),
		widget.NewFormItem("Steps", imageSteps),
		full(imgTestRow),
	)
	srvTab := tab(
		full(enableServer),
		full(serverExpose),
		widget.NewFormItem("Port", serverPort),
		widget.NewFormItem("Auth token", serverToken),
		widget.NewFormItem("Status", serverStatus),
	)
	smsTab := tab(
		full(enableSMS),
		widget.NewFormItem("Account SID", smsSID),
		widget.NewFormItem("Auth Token", smsToken),
		widget.NewFormItem("From number", smsFrom),
		widget.NewFormItem("Allowed numbers", smsAllowed),
		widget.NewFormItem("Poll interval (s)", smsPoll),
		widget.NewFormItem("Status", smsStatus),
		full(smsTestRow),
	)
	tgTab := tab(
		full(enableTelegram),
		widget.NewFormItem("Bot token", tgToken),
		widget.NewFormItem("Allowed username", tgUsername),
		widget.NewFormItem("Status", tgStatus),
		full(tgTestRow),
	)
	mxTab := tab(
		full(enableMatrix),
		widget.NewFormItem("Bot user ID", mxBotID),
		widget.NewFormItem("Password", mxPassword),
		widget.NewFormItem("Allowed user", mxAllowed),
		widget.NewFormItem("Status", mxStatus),
		full(mxTestRow),
	)
	apTab := tab(
		widget.NewFormItem("Theme", themeSel),
		full(autoSave),
	)

	tabs := container.NewAppTabs(
		container.NewTabItem("LLM", llmTab),
		container.NewTabItem("Speech-to-Text", sttTab),
		container.NewTabItem("Text-to-Speech", ttsTab),
		container.NewTabItem("Image Generation", imgTab),
		container.NewTabItem("Server", srvTab),
		container.NewTabItem("SMS", smsTab),
		container.NewTabItem("Telegram", tgTab),
		container.NewTabItem("Matrix", mxTab),
		container.NewTabItem("Appearance", apTab),
	)

	d := dialog.NewCustomWithoutButtons("Settings", tabs, win)
	d.Resize(fyne.NewSize(560, 640))
	d.SetOnClosed(func() {
		if testVoice != nil {
			testVoice.Stop()
			testVoice = nil
		}
		if onClosed != nil {
			onClosed()
		}
	})

	// ─── Save ──────────────────────────────────────────────────────────────
	save := func() {
		// Split the allowed-numbers textarea into a clean []string — trim each
		// line and drop blanks so a trailing newline isn't a phantom entry.
		var allowed []string
		for _, line := range strings.Split(smsAllowed.Text, "\n") {
			if v := strings.TrimSpace(line); v != "" {
				allowed = append(allowed, v)
			}
		}
		updated := settings.AppSettings{
			Theme:                   themeSel.Selected,
			APIEndpoint:             endpoint.Text,
			APIKey:                  apiKey.Text,
			Model:                   model.Text,
			FirstName:               firstName.Text,
			LastName:                lastName.Text,
			PetName:                 petName.Text,
			HistoryMessages:         intOr(historyMessages, current.HistoryMessages),
			STTEndpoint:             sttEndpoint.Text,
			STTAPIKey:               sttAPIKey.Text,
			STTModel:                sttModel.Text,
			ContinuousConversation:  continuousConv.Checked,
			EnableTTS:               enableTTS.Checked,
			TTSEndpoint:             ttsEndpoint.Text,
			TTSAPIKey:               ttsAPIKey.Text,
			TTSModel:                ttsModel.Text,
			TTSVoice:                ttsVoice.Text,
			InputDevice:             inputDevice.Selected,
			OutputDevice:            outputDevice.Selected,
			SaveToDisk:              autoSave.Checked,
			EnableImageGen:          enableImageGen.Checked,
			ComfyUIEndpoint:         comfyEndpoint.Text,
			ImageSteps:              intOr(imageSteps, current.ImageSteps),
			EnableServer:            enableServer.Checked,
			ServerExposeNetwork:     serverExpose.Checked,
			ServerPort:              intOr(serverPort, current.ServerPort),
			ServerAuthToken:         serverToken.Text,
			EnableSMS:               enableSMS.Checked,
			TwilioAccountSID:        smsSID.Text,
			TwilioAuthToken:         smsToken.Text,
			TwilioFromNumber:        smsFrom.Text,
			SMSAllowedNumbers:       allowed,
			SMSPollSeconds:          intOr(smsPoll, smsPollDefault),
			EnableTelegram:          enableTelegram.Checked,
			TelegramBotToken:        tgToken.Text,
			TelegramAllowedUsername: strings.TrimSpace(tgUsername.Text),
			EnableMatrix:            enableMatrix.Checked,
			MatrixBotUserID:         strings.TrimSpace(mxBotID.Text),
			MatrixPassword:          mxPassword.Text,
			MatrixAllowedUser:       strings.TrimSpace(mxAllowed.Text),
		}
		if err := updated.Save(); err != nil {
			dialog.ShowError(fmt.Errorf("could not save settings: %w", err), win)
			return
		}
		// Hot-reapply each service to the new config. The status labels update
		// so a failed bind/login shows without closing the dialog.
		serverStatus.SetText(srvMgr.Apply(updated))
		smsStatus.SetText(smsMgr.Apply(updated))
		tgStatus.SetText(tgMgr.Apply(updated))
		mxStatus.SetText(mxMgr.Apply(updated))
		// If any apply failed, keep the dialog open so the user can fix it.
		if strings.HasPrefix(serverStatus.Text, "✗") ||
			strings.HasPrefix(smsStatus.Text, "✗") ||
			strings.HasPrefix(tgStatus.Text, "✗") ||
			strings.HasPrefix(mxStatus.Text, "✗") {
			return
		}
		d.Hide()
	}

	saveBtn := widget.NewButton("Save", save)
	saveBtn.Importance = widget.HighImportance
	cancelBtn := widget.NewButton("Cancel", func() { d.Hide() })
	d.SetButtons([]fyne.CanvasObject{cancelBtn, saveBtn})

	// Initial population, matching the Qt dialog's open-time fetches.
	loadModels()
	loadSTTModels()
	loadTTSModels()
	refreshContext()

	d.Show()
}
