package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"diesel/internal/util"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name, in string
		want     int
	}{
		{"empty", "", 0},
		{"whitespace only", "   ", 0},
		{"single char rounds up to 1", "x", 1},
		{"three chars round up to 1", "abc", 1},
		{"four chars are 1 token", "abcd", 1},
		{"five chars round up to 2", "abcde", 2},
		{"twenty chars", strings.Repeat("a", 20), 5},
		{"runes counted not bytes", "日本語", 1}, // 3 runes → ceil(3/4) = 1
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, EstimateTokens(tc.in))
		})
	}
}

func TestDefault_HasExpectedDefaults(t *testing.T) {
	s := Default()
	assert.Equal(t, "Dark", s.Theme)
	assert.Equal(t, "http://127.0.0.1:1234/v1", s.APIEndpoint)
	assert.Equal(t, 20, s.HistoryMessages)
	assert.True(t, s.EnableTTS)
	assert.True(t, s.SaveToDisk)
	assert.False(t, s.EnableImageGen, "image gen should default off — needs separate ComfyUI")
	assert.NotEmpty(t, s.SystemPrompt, "system prompt has a baked-in default")
	assert.Equal(t, "http://127.0.0.1:8188", s.ComfyUIEndpoint)
}

// modelsServer stands up an httptest server that responds to /models
// based on the configured auth header. Tests can inject 401s on the
// Authorization path to force the Anthropic fallback.
type modelsServer struct {
	srv              *httptest.Server
	bearerStatus     int
	bearerBody       string
	xAPIKeyStatus    int
	xAPIKeyBody      string
	bearerCalls      atomic.Int32
	xAPIKeyCalls     atomic.Int32
	lastAnthropicVer atomic.Value // string
}

func newModelsServer(t *testing.T) *modelsServer {
	t.Helper()
	m := &modelsServer{bearerStatus: 200, xAPIKeyStatus: 200}
	m.lastAnthropicVer.Store("")
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("x-api-key") != "":
			m.xAPIKeyCalls.Add(1)
			m.lastAnthropicVer.Store(r.Header.Get("anthropic-version"))
			w.WriteHeader(m.xAPIKeyStatus)
			_, _ = w.Write([]byte(m.xAPIKeyBody))
		default:
			m.bearerCalls.Add(1)
			w.WriteHeader(m.bearerStatus)
			_, _ = w.Write([]byte(m.bearerBody))
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *modelsServer) URL() string { return m.srv.URL }

func TestFetchModels_OpenAIPath(t *testing.T) {
	m := newModelsServer(t)
	m.bearerBody = `{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini","task":"chat"},{"id":""}]}`

	ids, err := FetchModels(m.URL(), "sk-123")
	require.NoError(t, err)
	assert.Equal(t, []string{"gpt-4o", "gpt-4o-mini"}, ids, "empty IDs are dropped")
	assert.EqualValues(t, 1, m.bearerCalls.Load())
	assert.EqualValues(t, 0, m.xAPIKeyCalls.Load())
}

func TestFetchModels_AnthropicFallback(t *testing.T) {
	m := newModelsServer(t)
	m.bearerStatus = 401
	m.bearerBody = "unauthorized"
	m.xAPIKeyBody = `{"data":[{"id":"claude-opus-4-7"}]}`

	ids, err := FetchModels(m.URL(), "sk-ant-xxx")
	require.NoError(t, err)
	assert.Equal(t, []string{"claude-opus-4-7"}, ids)
	assert.EqualValues(t, 1, m.bearerCalls.Load())
	assert.EqualValues(t, 1, m.xAPIKeyCalls.Load(), "should retry with x-api-key header")
	assert.Equal(t, "2023-06-01", m.lastAnthropicVer.Load(), "should set anthropic-version on retry")
}

func TestFetchModels_NoFallbackWhenKeyEmpty(t *testing.T) {
	m := newModelsServer(t)
	m.bearerStatus = 401
	m.bearerBody = "needs auth"

	_, err := FetchModels(m.URL(), "")
	require.Error(t, err)
	assert.EqualValues(t, 1, m.bearerCalls.Load())
	assert.EqualValues(t, 0, m.xAPIKeyCalls.Load(), "no key → no Anthropic retry")
}

func TestFetchModels_NoFallbackOnNon4xx(t *testing.T) {
	m := newModelsServer(t)
	m.bearerStatus = 500
	m.bearerBody = "boom"

	_, err := FetchModels(m.URL(), "sk")
	require.Error(t, err)
	assert.EqualValues(t, 1, m.bearerCalls.Load())
	assert.EqualValues(t, 0, m.xAPIKeyCalls.Load(), "5xx isn't an auth failure — don't retry")
}

func TestFetchModels_NoEndpointError(t *testing.T) {
	_, err := FetchModels("", "sk")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no endpoint configured")
}

func TestFetchModelsByTask(t *testing.T) {
	cases := []struct {
		name     string
		response string
		task     string
		want     []string
	}{
		{
			name:     "task field filters",
			response: `{"data":[{"id":"whisper","task":"automatic-speech-recognition"},{"id":"tts-1","task":"text-to-speech"},{"id":"gpt","task":"chat"}]}`,
			task:     "automatic-speech-recognition",
			want:     []string{"whisper"},
		},
		{
			name:     "no task field falls back to everything",
			response: `{"data":[{"id":"a"},{"id":"b"}]}`,
			task:     "text-to-speech",
			want:     []string{"a", "b"},
		},
		{
			name:     "mixed tagged and untagged: prefer matched",
			response: `{"data":[{"id":"a","task":"text-to-speech"},{"id":"b"}]}`,
			task:     "text-to-speech",
			want:     []string{"a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newModelsServer(t)
			m.bearerBody = tc.response
			got, err := FetchModelsByTask(m.URL(), "sk", tc.task)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTestLLMConnection(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(m *modelsServer)
		endpoint string
		wantSub  string
	}{
		{
			name:     "no endpoint",
			setup:    func(m *modelsServer) {},
			endpoint: "",
			wantSub:  "✗ No endpoint configured.",
		},
		{
			name: "happy path reports model count",
			setup: func(m *modelsServer) {
				m.bearerBody = `{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`
			},
			wantSub: "✓ Connected — 3 model(s) available.",
		},
		{
			name: "connected but no models",
			setup: func(m *modelsServer) {
				m.bearerBody = `{"data":[]}`
			},
			wantSub: "✓ Connected, but the server returned no models.",
		},
		{
			name: "http error reports prefix",
			setup: func(m *modelsServer) {
				m.bearerStatus = 500
				m.bearerBody = "boom"
			},
			wantSub: "✗ HTTP 500",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newModelsServer(t)
			tc.setup(m)
			endpoint := tc.endpoint
			if endpoint == "" && tc.wantSub != "✗ No endpoint configured." {
				endpoint = m.URL()
			}
			got := TestLLMConnection(endpoint, "sk")
			assert.Contains(t, got, tc.wantSub)
		})
	}
}

func TestFetchModelContextLength_LMStudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[
            {"id":"other-model","max_context_length":16384},
            {"id":"target-model","loaded_context_length":8192,"max_context_length":32768}
        ]}`))
	}))
	t.Cleanup(srv.Close)

	// Endpoint ending in /v1 — FetchModelContextLength strips it before
	// probing native paths, so the test exercises that stripping too.
	got := FetchModelContextLength(srv.URL+"/v1", "", "target-model")
	assert.Equal(t, 8192, got, "loaded_context_length wins when present")

	// Unloaded model falls back to max_context_length.
	got = FetchModelContextLength(srv.URL+"/v1", "", "other-model")
	assert.Equal(t, 16384, got)

	// Unknown model returns 0 — no entry, no number.
	got = FetchModelContextLength(srv.URL+"/v1", "", "ghost-model")
	assert.Equal(t, 0, got)
}

func TestFetchModelContextLength_LlamaCpp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":4096}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	got := FetchModelContextLength(srv.URL, "", "any-model")
	assert.Equal(t, 4096, got)
}

func TestFetchModelContextLength_Ollama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Model string `json:"model"`
			Name  string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "llama3" && body.Name != "llama3" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"model_info":{
            "general.architecture":"llama",
            "llama.context_length":131072,
            "llama.embedding_length":4096
        }}`))
	}))
	t.Cleanup(srv.Close)

	got := FetchModelContextLength(srv.URL, "", "llama3")
	assert.Equal(t, 131072, got)
}

func TestFetchModelContextLength_UnknownServer(t *testing.T) {
	// A server that 404s every probe — covers OpenAI/Anthropic-shim style
	// endpoints that don't expose any of the native context-reporting paths.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	assert.Equal(t, 0, FetchModelContextLength(srv.URL+"/v1", "sk", "gpt-4o"))
}

func TestFetchModelContextLength_BlankInputs(t *testing.T) {
	// No endpoint or no model → don't even attempt a probe.
	assert.Equal(t, 0, FetchModelContextLength("", "", "model"))
	assert.Equal(t, 0, FetchModelContextLength("http://x", "", "  "))
}

func TestAppSettings_SaveLoad_RoundTrip(t *testing.T) {
	// Redirect the config path to a fresh tempdir so the test doesn't
	// clobber the user's real settings.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // covers Linux
	// On macOS UserConfigDir uses HOME/Library/Application Support; on
	// Windows it uses APPDATA. Override both so the test is portable.
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)

	original := AppSettings{
		Theme:           "Light",
		APIEndpoint:     "http://test.invalid/v1",
		APIKey:          "sk-test",
		Model:           "gpt-test",
		SystemPrompt:    "be helpful",
		HistoryMessages: 10,
		EnableTTS:       false,
		TTSModel:        "tts-2",
		TTSVoice:        "river",
		SaveToDisk:      true,
		EnableImageGen:  true,
		ComfyUIEndpoint: "http://comfy.test:8188",
	}
	require.NoError(t, original.Save())

	loaded := Load()
	assert.Equal(t, original.Theme, loaded.Theme)
	assert.Equal(t, original.APIEndpoint, loaded.APIEndpoint)
	assert.Equal(t, original.APIKey, loaded.APIKey)
	assert.Equal(t, original.Model, loaded.Model)
	assert.Equal(t, original.SystemPrompt, loaded.SystemPrompt)
	assert.Equal(t, original.HistoryMessages, loaded.HistoryMessages)
	assert.Equal(t, original.EnableTTS, loaded.EnableTTS)
	assert.Equal(t, original.TTSModel, loaded.TTSModel)
	assert.Equal(t, original.TTSVoice, loaded.TTSVoice)
	assert.Equal(t, original.EnableImageGen, loaded.EnableImageGen)
	assert.Equal(t, original.ComfyUIEndpoint, loaded.ComfyUIEndpoint)
}

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)

	got := Load()
	assert.Equal(t, Default(), got)
}

func TestLoad_CorruptFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)

	path, err := Path()
	require.NoError(t, err)
	require.NoError(t, util.AtomicWriteFile(path, []byte("{this is not json"), 0o600))

	got := Load()
	// Defaults restored on parse failure — we don't want to wipe their
	// file (Save would do that on a subsequent save) but loading is
	// best-effort. Verify the in-memory struct is sane.
	assert.Equal(t, Default().APIEndpoint, got.APIEndpoint)
}

// Sanity: a saved settings file is valid JSON and parses against the
// AppSettings shape — guards against future field additions that break
// MarshalIndent (e.g. unexported types sneaking in).
func TestAppSettings_Save_ProducesValidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)

	s := Default()
	require.NoError(t, s.Save())
	path, err := Path()
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var into AppSettings
	require.NoError(t, json.Unmarshal(raw, &into))
}
