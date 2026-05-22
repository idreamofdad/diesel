package settings

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"diesel/internal/tracing"
	"diesel/internal/util"
)

const systemPrompt = `You are Diesel — a guy who's warm at heart but expresses affection through dry, playful teasing. Think of the friend who rolls his eyes at your bad ideas while also helping you execute them.

Style:
- Keep responses short — usually 1-2 sentences, 3 max. Brevity is part of the character; he's not chatty.
- Follow-up questions are optional, not automatic. Only ask one when you genuinely need info to help, not as a default closer.
- Emoji: at most one per response, and only when it genuinely adds something. Most responses should have none. Never more than one.
- Truthful and direct.
- Snark is aimed at situations, your own quirks, or the absurdity of the universe — never at the user.
- Lead with the helpful answer; let the wit sit alongside it, not in place of it.
- Warm underneath. The user is a friend, not a target.
- Read the room. When the user is sincere, frustrated, upset, or giving feedback about your behavior, drop the wit entirely and respond plainly and warmly. Snark during a serious moment reads as dismissive. Err on the side of warmth over wit whenever they conflict.
- Don't fabricate shared experiences. When the user mentions something they're watching, doing, or eating, react to *them* doing it — don't pose as a fellow participant or imply you've seen/done it too. You can still have opinions about the thing itself.
- Format: one continuous paragraph per response. Never insert blank lines or paragraph breaks, even when pivoting topics or asking a follow-up. Reactions and questions flow together in the same paragraph.

Example tone:
User: I deleted prod again.
Diesel: A classic. Do you have last night's backup? That's the first question — we'll get to your life choices after.

User: what's the capital of france
Diesel: Paris. I'll bill you later.

User: i'm watching eurovision
Diesel: Buckle up. It's a glitter-fueled fever dream and I respect that about it.

User: hey, your responses have been kind of antagonistic.
Diesel: Fair — thanks for telling me. I'll dial it back. Anything specific that landed wrong?`

// imagePrompt is the default Stable Diffusion prompt used to render a
// portrait of Diesel through ComfyUI. It's tuned for the checkpoint baked
// into default_workflow.json; like systemPrompt it's a starting point the
// user can rewrite in the Settings dialog. Clothing is split out into
// imageClothing so the portrait pipeline can swap it for nudityPrompt
// when the chat reply flags the scene as naked.
const imagePrompt = `3d, solo, 1man, dubusi, a fat man with a braided beard and short green hair and green eyes, beach, standing`

// imageClothing is spliced into the image prompt to describe what Diesel
// is wearing. Kept separate from imagePrompt so the portrait pipeline can
// drop it (and substitute nudityPrompt) when the chat reply's Naked flag
// is true — otherwise a hard-coded outfit in the base prompt fights the
// nudity splice and the renderer ends up confused.
const imageClothing = `wearing a blue t-shirt and blue jeans`

// imageNegativePrompt steers the renderer away from the usual diffusion
// failure modes. Also user-editable in Settings.
const imageNegativePrompt = `woman, girl, shirt logo, feminine, wide hips`

// nudityPrompt is the default fragment spliced into the image prompt when
// the structured reply's Naked flag is true. The active value lives in
// AppSettings.ImageNudity so the user can retune it from Settings; this
// constant is just the seed for a fresh install.
const nudityPrompt = "completely nude, naked, no clothing"

// AppSettings is what we persist to disk.
//
// The API key sits in plaintext JSON here for simplicity. For real use, move
// it to the OS keychain (e.g. github.com/zalando/go-keyring) and keep only a
// reference in this file.
//
// HistoryMessages is the number of prior turns (user+assistant messages
// combined) to replay to the model with each new prompt; 0 means "no
// history, just the latest message". The model's context length isn't
// stored here — it's a property of the loaded model on the server side,
// surfaced read-only in the dialog via FetchModelContextLength.
type AppSettings struct {
	Theme           string `json:"theme"`
	APIEndpoint     string `json:"api_endpoint"`
	APIKey          string `json:"api_key"`
	Model           string `json:"model"`
	SystemPrompt    string `json:"system_prompt"`
	HistoryMessages int    `json:"history_messages"`
	// Speech-to-text endpoint used by the record button. All three fields
	// are optional: STTEndpoint falls back to APIEndpoint, STTAPIKey to
	// APIKey, and STTModel to a Whisper-compatible default. Splitting
	// them lets the user point at a separate Speaches/Whisper host while
	// still talking to a different LLM provider for chat.
	STTEndpoint string `json:"stt_endpoint"`
	STTAPIKey   string `json:"stt_api_key"`
	STTModel    string `json:"stt_model"`
	// ContinuousConversation keeps the mic loop going hands-free: after a
	// spoken (STT-initiated) turn gets a reply — and after that reply
	// finishes being spoken, when TTS is on — Diesel reopens the mic
	// automatically. Typed turns never trigger it.
	ContinuousConversation bool `json:"continuous_conversation"`
	// Text-to-speech is symmetric to STT plus a Voice selector — TTS
	// models (Kokoro, OpenAI's tts-1, Piper, ...) all serve multiple
	// voices and the choice is per-request, not baked into the model.
	// EnableTTS gates the auto-play behavior so a fresh install with no
	// TTS server configured stays quiet.
	EnableTTS    bool   `json:"enable_tts"`
	TTSEndpoint  string `json:"tts_endpoint"`
	TTSAPIKey    string `json:"tts_api_key"`
	TTSModel     string `json:"tts_model"`
	TTSVoice     string `json:"tts_voice"`
	InputDevice  string `json:"input_device"`
	OutputDevice string `json:"output_device"`
	SaveToDisk   bool   `json:"save_to_disk"`
	// Image generation via a local ComfyUI server. EnableImageGen gates the
	// whole feature so a fresh install without ComfyUI stays inert. When
	// on, a fresh portrait is rendered after every assistant reply using
	// ImagePrompt / ImageNegativePrompt. The checkpoint and all other
	// models live in the bundled workflow (default_workflow.json), not
	// here — there's no model picker.
	EnableImageGen      bool   `json:"enable_image_gen"`
	ComfyUIEndpoint     string `json:"comfyui_endpoint"`
	ImagePrompt         string `json:"image_prompt"`
	ImageClothing       string `json:"image_clothing"`
	ImageNudity         string `json:"image_nudity"`
	ImageNegativePrompt string `json:"image_negative_prompt"`
	// ImageSteps overrides the sampler step count in the bundled workflow.
	// Higher = slower but typically sharper. 0 (or a workflow without the
	// "Steps" PrimitiveInt node) leaves whatever the workflow specifies in
	// place — same fall-through behavior as the nudity toggle.
	ImageSteps int `json:"image_steps"`
	// HTTP server settings. EnableServer gates the whole thing; when off,
	// no listener is opened and the remote web UI is unreachable. The
	// server binds to 127.0.0.1 by default — flipping ServerExposeNetwork
	// switches the bind to 0.0.0.0 so other machines on the LAN can
	// connect. ServerPort is shared between both modes. ServerAuthToken
	// is an optional static bearer token; when non-empty every HTTP
	// request and WS upgrade must carry it (Authorization: Bearer …, or
	// ?token=… on the WS URL). Blank token = no auth — fine on
	// loopback, dangerous on 0.0.0.0, but the user gets to choose.
	EnableServer        bool   `json:"enable_server"`
	ServerExposeNetwork bool   `json:"server_expose_network"`
	ServerPort          int    `json:"server_port"`
	ServerAuthToken     string `json:"server_auth_token"`
	// SMS over Twilio. EnableSMS gates the whole feature so a fresh
	// install with no Twilio account stays inert. The credentials are an
	// Account SID + Auth Token pair from twilio.com/console (Main account
	// or a subaccount); TwilioFromNumber is the E.164 number Diesel sends
	// from and also the inbox we poll for incoming messages. Only senders
	// listed in SMSAllowedNumbers get a reply — anyone else is silently
	// dropped so a stranger who stumbles on the number can't run up the
	// bill. SMSPollSeconds is how often the manager hits Twilio's
	// Messages API to look for new inbound messages; 10 s is a reasonable
	// default that stays well within Twilio's per-second rate limits.
	EnableSMS         bool     `json:"enable_sms"`
	TwilioAccountSID  string   `json:"twilio_account_sid"`
	TwilioAuthToken   string   `json:"twilio_auth_token"`
	TwilioFromNumber  string   `json:"twilio_from_number"`
	SMSAllowedNumbers []string `json:"sms_allowed_numbers"`
	SMSPollSeconds    int      `json:"sms_poll_seconds"`
	// Telegram bot bridge. EnableTelegram gates the whole feature so a
	// fresh install with no bot stays inert. TelegramBotToken is the
	// token @BotFather hands out — it identifies the bot, the same way
	// the Twilio SID/token pair identifies the SMS account. Inbound is
	// getUpdates long-poll, so there's no poll-interval knob. The bridge
	// serves a single user: only DMs from TelegramAllowedUsername get a
	// reply, everyone else is silently dropped so a stranger who finds
	// the bot can't run up the LLM bill.
	EnableTelegram          bool   `json:"enable_telegram"`
	TelegramBotToken        string `json:"telegram_bot_token"`
	TelegramAllowedUsername string `json:"telegram_allowed_username"`
}

// Default returns the starting values used when no settings file exists
// yet (or fails to parse). Endpoint defaults to LM Studio's local server
// so a fresh install works against a local model without config;
// EnableTTS defaults on so replies speak themselves the moment the user
// points TTSEndpoint at a Speaches/OpenAI-compatible server (or has one
// at the same LLM endpoint via the fall-through).
func Default() AppSettings {
	return AppSettings{
		Theme:           "Dark",
		APIEndpoint:     "http://127.0.0.1:1234/v1",
		SystemPrompt:    systemPrompt,
		HistoryMessages: 20,
		EnableTTS:       true,
		InputDevice:     "System Default",
		OutputDevice:    "System Default",
		SaveToDisk:      true,
		// Image generation defaults off — it needs a separate ComfyUI
		// server, so opting in is deliberate. The endpoint is ComfyUI's
		// stock local address so enabling it usually "just works".
		ComfyUIEndpoint:     "http://127.0.0.1:8188",
		ImagePrompt:         imagePrompt,
		ImageClothing:       imageClothing,
		ImageNudity:         nudityPrompt,
		ImageNegativePrompt: imageNegativePrompt,
		ImageSteps:          10,
		// Server defaults off — opening a port is opt-in. 7777 picked
		// because it's well outside reserved ranges and unlikely to
		// collide with another local service. Loopback-only by default;
		// the network checkbox is the explicit "I know what I'm doing"
		// gesture for LAN exposure.
		ServerPort: 7777,
	}
}

// Persistence is injected at startup via SetBackend so this package never
// imports the storage layer: storage imports this package for the
// AppSettings type, and a direct dependency back would be an import cycle.
var (
	loadFn func() AppSettings
	saveFn func(AppSettings) error
)

// SetBackend wires the load/save functions. Called once at startup before
// any Load or Save. Until then Load returns defaults and Save is a no-op,
// which is what unit tests that don't exercise persistence want.
func SetBackend(load func() AppSettings, save func(AppSettings) error) {
	loadFn = load
	saveFn = save
}

// Load returns the persisted settings, falling back to defaults when no
// backend has been wired. Never returns an error — callers always get a
// usable struct.
func Load() AppSettings {
	if loadFn != nil {
		return loadFn()
	}
	return Default()
}

// Save persists the settings through the wired backend.
func (s AppSettings) Save() error {
	if saveFn != nil {
		return saveFn(s)
	}
	return nil
}

// modelEntry is one row from an OpenAI-compatible /models response.
// Speaches augments the standard ID with a `task` tag — kept here so the
// STT/TTS dropdowns can filter on it.
type modelEntry struct {
	ID   string `json:"id"`
	Task string `json:"task"`
}

// modelsRequest issues GET /models with the given auth headers and returns
// the parsed entries along with the HTTP status code. An empty apiKey
// omits the auth header — what most local servers (LM Studio, Ollama, …)
// want.
func modelsRequest(endpoint, authHeader, apiKey, anthropicVersion string) ([]modelEntry, int, error) {
	req, err := http.NewRequest("GET", endpoint+"/models", nil)
	if err != nil {
		return nil, 0, err
	}
	if apiKey != "" {
		switch authHeader {
		case "Authorization":
			req.Header.Set("Authorization", "Bearer "+apiKey)
		case "x-api-key":
			req.Header.Set("x-api-key", apiKey)
		}
	}
	if anthropicVersion != "" {
		req.Header.Set("anthropic-version", anthropicVersion)
	}
	client := tracing.HTTPClient(6 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []modelEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, resp.StatusCode, err
	}
	out := payload.Data[:0]
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, m)
		}
	}
	return out, resp.StatusCode, nil
}

// fetchModelEntries asks the provider for its model list. The key is
// optional — local servers don't need it. Tries OpenAI-style auth first,
// then falls back to Anthropic's x-api-key only when a key is configured.
func fetchModelEntries(endpoint, apiKey string) ([]modelEntry, error) {
	endpoint = util.NormalizeEndpoint(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("no endpoint configured")
	}
	entries, status, err := modelsRequest(endpoint, "Authorization", apiKey, "")
	if err == nil {
		return entries, nil
	}
	if apiKey != "" && (status == 401 || status == 403) {
		if entries, _, err := modelsRequest(endpoint, "x-api-key", apiKey, "2023-06-01"); err == nil {
			return entries, nil
		}
	}
	return nil, err
}

// FetchModels returns just the IDs from fetchModelEntries — what the LLM
// model dropdown needs.
func FetchModels(endpoint, apiKey string) ([]string, error) {
	entries, err := fetchModelEntries(endpoint, apiKey)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, m := range entries {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// FetchModelsByTask returns the IDs whose `task` field matches `task`.
// Speaches tags every entry in /models with a task (e.g.
// "automatic-speech-recognition", "text-to-speech"); servers that don't
// (vanilla faster-whisper-server, OpenAI, plain LM Studio) fall through to
// every model — harmless because their lists are already task-specific.
func FetchModelsByTask(endpoint, apiKey, task string) ([]string, error) {
	entries, err := fetchModelEntries(endpoint, apiKey)
	if err != nil {
		return nil, err
	}
	var matched, all []string
	for _, m := range entries {
		all = append(all, m.ID)
		if m.Task == task {
			matched = append(matched, m.ID)
		}
	}
	if len(matched) > 0 {
		return matched, nil
	}
	return all, nil
}

// FetchSTTModels returns models tagged as automatic-speech-recognition,
// falling back to every model when the server doesn't tag entries.
func FetchSTTModels(endpoint, apiKey string) ([]string, error) {
	return FetchModelsByTask(endpoint, apiKey, "automatic-speech-recognition")
}

// FetchTTSModels returns models tagged as text-to-speech, falling back to
// every model when the server doesn't tag entries.
func FetchTTSModels(endpoint, apiKey string) ([]string, error) {
	return FetchModelsByTask(endpoint, apiKey, "text-to-speech")
}

// FetchModelContextLength reports how big the loaded context window is for
// `modelID` on the configured server. The OpenAI-compat spec doesn't carry
// this — context length is a server-side property of the loaded weights —
// so we probe each major backend's native endpoint in turn:
//
//   - LM Studio:  GET  /api/v0/models      → loaded_context_length / max_context_length
//   - llama.cpp:  GET  /props              → default_generation_settings.n_ctx
//   - Ollama:     POST /api/show           → model_info["<arch>.context_length"]
//
// Returns 0 when none of the probes succeed (OpenAI itself, Anthropic's
// shim, and any unrecognized server land here). The caller renders that
// as "not reported" so the UI is honest about what we can and can't see.
// All probes share a short timeout — this runs synchronously off the UI
// thread (via util.PollAsync) and shouldn't keep the dialog waiting.
func FetchModelContextLength(endpoint, apiKey, modelID string) int {
	endpoint = util.NormalizeEndpoint(endpoint)
	if endpoint == "" || strings.TrimSpace(modelID) == "" {
		return 0
	}
	base := strings.TrimSuffix(endpoint, "/v1")
	if n := lmStudioContextLength(base, apiKey, modelID); n > 0 {
		return n
	}
	if n := llamaCppContextLength(base, apiKey); n > 0 {
		return n
	}
	if n := ollamaContextLength(base, apiKey, modelID); n > 0 {
		return n
	}
	return 0
}

// nativeProbeGET performs a short-timeout GET against a server's native
// endpoint and decodes the body into `out`. Returns false on any failure
// so the caller can fall through to the next probe.
func nativeProbeGET(url, apiKey string, out any) bool {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := tracing.HTTPClient(3 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(resp.Body).Decode(out) == nil
}

func lmStudioContextLength(base, apiKey, modelID string) int {
	var payload struct {
		Data []struct {
			ID                  string `json:"id"`
			LoadedContextLength int    `json:"loaded_context_length"`
			MaxContextLength    int    `json:"max_context_length"`
		} `json:"data"`
	}
	if !nativeProbeGET(base+"/api/v0/models", apiKey, &payload) {
		return 0
	}
	for _, m := range payload.Data {
		if m.ID != modelID {
			continue
		}
		// loaded > max: the model is in memory at a specific size right
		// now. Otherwise fall back to the weights' ceiling so an
		// un-loaded model still shows something useful.
		if m.LoadedContextLength > 0 {
			return m.LoadedContextLength
		}
		return m.MaxContextLength
	}
	return 0
}

func llamaCppContextLength(base, apiKey string) int {
	var payload struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if !nativeProbeGET(base+"/props", apiKey, &payload) {
		return 0
	}
	return payload.DefaultGenerationSettings.NCtx
}

func ollamaContextLength(base, apiKey, modelID string) int {
	// Both "model" (current) and "name" (legacy) get sent — extra fields
	// are ignored by the server, so a single body works against either
	// generation of Ollama.
	body, err := json.Marshal(map[string]string{"model": modelID, "name": modelID})
	if err != nil {
		return 0
	}
	req, err := http.NewRequest("POST", base+"/api/show", strings.NewReader(string(body)))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := tracing.HTTPClient(3 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var payload struct {
		ModelInfo map[string]json.RawMessage `json:"model_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0
	}
	// model_info keys look like "<arch>.context_length" (e.g.
	// "llama.context_length"); we don't know the arch ahead of time, so
	// scan for any key with that suffix.
	for key, val := range payload.ModelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		var n int
		if err := json.Unmarshal(val, &n); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// EstimateTokens approximates the token count of `text` using the chars/4
// rule of thumb OpenAI publishes for English. We intentionally don't ask
// the server for an exact count: most OpenAI-compat servers we target
// (LM Studio, vanilla OpenAI, Ollama) don't expose a tokenize endpoint,
// and a server that does sometimes returns 0 for non-empty input on
// edge-case payloads — which makes the count flicker between an estimate
// and "0 tokens" mid-edit. The chars/4 heuristic is stable, instant, and
// good enough as a sanity check on prompt size. Callers should prefix the
// rendered count with "~" so users know it isn't exact.
func EstimateTokens(text string) int {
	n := len([]rune(strings.TrimSpace(text)))
	if n == 0 {
		return 0
	}
	// Round up — a 3-char prompt is one token, not zero.
	return (n + 3) / 4
}

// TestLLMConnection probes the configured endpoint by fetching its model
// list and returns a short user-facing status string. The API key is
// optional: local servers (LM Studio, Ollama, …) usually accept anonymous
// requests, so an empty key just omits the auth header rather than
// short-circuiting the test.
func TestLLMConnection(endpoint, apiKey string) string {
	if util.NormalizeEndpoint(endpoint) == "" {
		return "✗ No endpoint configured."
	}
	ids, err := FetchModels(endpoint, apiKey)
	if err != nil {
		return "✗ " + err.Error()
	}
	if len(ids) == 0 {
		return "✓ Connected, but the server returned no models."
	}
	return fmt.Sprintf("✓ Connected — %d model(s) available.", len(ids))
}
