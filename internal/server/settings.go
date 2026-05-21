package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"diesel/internal/comfyui"
	"diesel/internal/settings"
	"diesel/internal/tts"
	"diesel/internal/util"

	"github.com/gin-gonic/gin"
)

// secretMask is the placeholder substituted for stored API keys when
// settings are sent to the web client. Save flows treat an incoming
// field equal to this sentinel as "unchanged" and reuse the saved
// value, so the real key never crosses the wire on a round-trip.
const secretMask = "********"

// maskSettings copies s and replaces every secret with secretMask when
// the underlying value is non-empty. Used both by GET /api/v1/settings and
// by the response of POST /api/v1/settings.
func maskSettings(s settings.AppSettings) settings.AppSettings {
	if s.APIKey != "" {
		s.APIKey = secretMask
	}
	if s.STTAPIKey != "" {
		s.STTAPIKey = secretMask
	}
	if s.TTSAPIKey != "" {
		s.TTSAPIKey = secretMask
	}
	if s.ServerAuthToken != "" {
		s.ServerAuthToken = secretMask
	}
	if s.TwilioAuthToken != "" {
		s.TwilioAuthToken = secretMask
	}
	if s.TelegramBotToken != "" {
		s.TelegramBotToken = secretMask
	}
	return s
}

// mergeFromWeb reconciles an incoming web payload with the on-disk
// settings:
//   - Server-tab fields (port/bind/token/enable) are NOT writable from
//     the web client and are always taken from `current`.
//   - Audio device pickers are hardware on the host, not the browser,
//     so those also stay locked to `current`.
//   - Secret fields equal to secretMask are taken from `current` (the
//     user didn't touch the masked input).
func mergeFromWeb(current, incoming settings.AppSettings) settings.AppSettings {
	incoming.EnableServer = current.EnableServer
	incoming.ServerExposeNetwork = current.ServerExposeNetwork
	incoming.ServerPort = current.ServerPort
	incoming.ServerAuthToken = current.ServerAuthToken
	incoming.InputDevice = current.InputDevice
	incoming.OutputDevice = current.OutputDevice
	// SMS/Twilio config lives on the host — credentials are bound to a
	// physical account/number, not something the remote browser should
	// be allowed to retune. Preserve verbatim from the on-disk copy.
	incoming.EnableSMS = current.EnableSMS
	incoming.TwilioAccountSID = current.TwilioAccountSID
	incoming.TwilioAuthToken = current.TwilioAuthToken
	incoming.TwilioFromNumber = current.TwilioFromNumber
	incoming.SMSAllowedNumbers = current.SMSAllowedNumbers
	incoming.SMSPollSeconds = current.SMSPollSeconds
	// Telegram is the same shape — the bot token is host-bound, not
	// something a remote browser should retune. Preserve verbatim.
	incoming.EnableTelegram = current.EnableTelegram
	incoming.TelegramBotToken = current.TelegramBotToken
	incoming.TelegramAllowedUsername = current.TelegramAllowedUsername

	if incoming.APIKey == secretMask {
		incoming.APIKey = current.APIKey
	}
	if incoming.STTAPIKey == secretMask {
		incoming.STTAPIKey = current.STTAPIKey
	}
	if incoming.TTSAPIKey == secretMask {
		incoming.TTSAPIKey = current.TTSAPIKey
	}
	return incoming
}

// resolveSecret returns the real key for a probe call when the web
// client sent the mask sentinel (meaning "use what's on disk"). Used by
// the test/probe endpoints so the user can verify a saved key without
// having to retype it.
func resolveSecret(incoming, saved string) string {
	if incoming == secretMask {
		return saved
	}
	return incoming
}

// handleSettingsGet returns the on-disk settings with secrets masked.
func (m *Manager) handleSettingsGet(c *gin.Context) {
	c.JSON(http.StatusOK, maskSettings(settings.Load()))
}

// handleSettingsSave persists the merged settings and re-applies the
// server config (which is a no-op here because the web client can't
// change server fields, but kept for symmetry with the desktop dialog).
// Returns the saved settings, masked, so the caller can refresh its
// form state in one round-trip.
func (m *Manager) handleSettingsSave(c *gin.Context) {
	var incoming settings.AppSettings
	if err := c.BindJSON(&incoming); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	current := settings.Load()
	merged := mergeFromWeb(current, incoming)
	if err := merged.Save(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// The server-tab fields are preserved verbatim, so Apply is a no-op
	// today. Still call it so a future relaxation of the "no server tab
	// from the web" rule doesn't silently skip the restart.
	m.Apply(merged)
	c.JSON(http.StatusOK, maskSettings(merged))
}

// probeRequest is the body for /api/v1/settings/probe and the test
// endpoints. Most fields are optional — only the ones the kind cares
// about are read.
type probeRequest struct {
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
	Voice    string `json:"voice"`
	Text     string `json:"text"`
}

// resolveEndpoint mirrors the fall-through the runtime uses when an
// STT/TTS endpoint is blank — fall back to the main API endpoint so
// the test reflects what the live request would actually hit.
func resolveEndpoint(specific, saved settings.AppSettings, kind string) (string, string) {
	switch kind {
	case "llm":
		return util.FirstNonEmpty(specific.APIEndpoint, saved.APIEndpoint),
			util.FirstNonEmpty(specific.APIKey, saved.APIKey)
	case "stt":
		ep := util.FirstNonEmpty(specific.STTEndpoint,
			util.FirstNonEmpty(specific.APIEndpoint, saved.APIEndpoint))
		key := util.FirstNonEmpty(specific.STTAPIKey,
			util.FirstNonEmpty(specific.APIKey, saved.APIKey))
		return ep, key
	case "tts":
		ep := util.FirstNonEmpty(specific.TTSEndpoint,
			util.FirstNonEmpty(specific.APIEndpoint, saved.APIEndpoint))
		key := util.FirstNonEmpty(specific.TTSAPIKey,
			util.FirstNonEmpty(specific.APIKey, saved.APIKey))
		return ep, key
	}
	return "", ""
}

// handleSettingsModels fetches the model list from a service for the
// settings form's dropdowns. The kind discriminates which fallback
// chain to use; the masked keys in `req` are resolved against saved
// settings before the upstream call.
func (m *Manager) handleSettingsModels(c *gin.Context) {
	var req probeRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	saved := settings.Load()
	// Unmask secrets so a probe of "the saved key" works without
	// requiring the user to retype it.
	req.APIKey = resolveSecret(req.APIKey, savedKeyFor(saved, req.Kind))
	resolved := settings.AppSettings{
		APIEndpoint: req.Endpoint, APIKey: req.APIKey,
		STTEndpoint: req.Endpoint, STTAPIKey: req.APIKey,
		TTSEndpoint: req.Endpoint, TTSAPIKey: req.APIKey,
	}
	ep, key := resolveEndpoint(resolved, saved, req.Kind)

	var (
		ids []string
		err error
	)
	switch req.Kind {
	case "llm":
		ids, err = settings.FetchModels(ep, key)
	case "stt":
		ids, err = settings.FetchSTTModels(ep, key)
	case "tts":
		ids, err = settings.FetchTTSModels(ep, key)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown kind"})
		return
	}
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"models": []string{}, "error": err.Error()})
		return
	}
	out := gin.H{"models": ids}
	// Context length only makes sense for the LLM and only with a model
	// selected. Probed inline rather than as a separate endpoint so the
	// dropdown-refresh round-trip also surfaces the new ctx size.
	if req.Kind == "llm" && strings.TrimSpace(req.Model) != "" {
		out["context_length"] = settings.FetchModelContextLength(ep, key, req.Model)
	}
	c.JSON(http.StatusOK, out)
}

// savedKeyFor picks the saved secret that corresponds to the kind so
// resolveSecret has something to fall back to.
func savedKeyFor(s settings.AppSettings, kind string) string {
	switch kind {
	case "stt":
		return util.FirstNonEmpty(s.STTAPIKey, s.APIKey)
	case "tts":
		return util.FirstNonEmpty(s.TTSAPIKey, s.APIKey)
	}
	return s.APIKey
}

// handleSettingsTest probes a service and returns a short status string
// — the same one the desktop dialog renders next to the Test button.
// TTS testing has its own endpoint (handleSettingsTestTTS) because the
// payload is audio, not text.
func (m *Manager) handleSettingsTest(c *gin.Context) {
	var req probeRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	saved := settings.Load()
	req.APIKey = resolveSecret(req.APIKey, savedKeyFor(saved, req.Kind))
	resolved := settings.AppSettings{
		APIEndpoint: req.Endpoint, APIKey: req.APIKey,
		STTEndpoint: req.Endpoint, STTAPIKey: req.APIKey,
		TTSEndpoint: req.Endpoint, TTSAPIKey: req.APIKey,
	}
	switch req.Kind {
	case "llm":
		ep, key := resolveEndpoint(resolved, saved, "llm")
		c.JSON(http.StatusOK, gin.H{"status": settings.TestLLMConnection(ep, key)})
	case "stt":
		ep, key := resolveEndpoint(resolved, saved, "stt")
		if strings.TrimSpace(ep) == "" {
			c.JSON(http.StatusOK, gin.H{"status": "✗ No endpoint configured."})
			return
		}
		ids, err := settings.FetchSTTModels(ep, key)
		switch {
		case err != nil:
			c.JSON(http.StatusOK, gin.H{"status": "✗ " + err.Error()})
		case len(ids) == 0:
			c.JSON(http.StatusOK, gin.H{"status": "✓ Connected, but the server returned no models."})
		default:
			c.JSON(http.StatusOK, gin.H{"status": shortCount(ids)})
		}
	case "image":
		c.JSON(http.StatusOK, gin.H{"status": comfyui.TestConnection(req.Endpoint)})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown kind"})
	}
}

func shortCount(ids []string) string {
	if len(ids) == 1 {
		return "✓ Connected — 1 model available."
	}
	return "✓ Connected — multiple models available."
}

// handleSettingsTestTTS synthesizes a short sample so the user can
// hear the configured voice before saving. On success the raw audio
// bytes (WAV) are streamed back for the browser to play through an
// <audio> element. On failure we return JSON so the form can render
// the error inline.
func (m *Manager) handleSettingsTestTTS(c *gin.Context) {
	var req probeRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	saved := settings.Load()
	req.APIKey = resolveSecret(req.APIKey, util.FirstNonEmpty(saved.TTSAPIKey, saved.APIKey))
	resolved := settings.AppSettings{
		APIEndpoint: req.Endpoint, APIKey: req.APIKey,
		TTSEndpoint: req.Endpoint, TTSAPIKey: req.APIKey,
	}
	ep, key := resolveEndpoint(resolved, saved, "tts")
	if strings.TrimSpace(ep) == "" {
		c.JSON(http.StatusOK, gin.H{"error": "✗ No endpoint configured."})
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		text = "Testing, one two three."
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	audioBytes, err := tts.Synthesize(ctx, ep, key, req.Model, req.Voice, text)
	if err != nil {
		// Surface as a 200 with an error field so the browser can read
		// the message — a 502 would also work but forces fetch() callers
		// to special-case the body parse.
		c.JSON(http.StatusOK, gin.H{"error": "✗ " + err.Error()})
		return
	}
	c.Data(http.StatusOK, "audio/wav", audioBytes)
}
