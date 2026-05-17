package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chatRequest is what we expect to receive at the /chat/completions
// endpoint — used to assert the marshaled body matches the configured
// history window and system prompt.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

// stubChatServer spins up an httptest server that returns `body` with
// status `status` and captures the decoded request body so tests can
// inspect what the client sent.
func stubChatServer(t *testing.T, status int, body string) (*httptest.Server, *chatRequest) {
	t.Helper()
	captured := &chatRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, captured)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// jsonChoice wraps `content` in the OpenAI /chat/completions response
// envelope so test cases can stay focused on the message payload.
func jsonChoice(content string, usage tokenUsage) string {
	envelope := map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{"content": content},
		}},
		"usage": usage,
	}
	out, _ := json.Marshal(envelope)
	return string(out)
}

func TestChatCompletion_ConfigurationErrors(t *testing.T) {
	cases := []struct {
		name     string
		settings AppSettings
		wantErr  string
	}{
		{
			name:     "missing endpoint",
			settings: AppSettings{Model: "anything"},
			wantErr:  "no endpoint configured",
		},
		{
			name:     "endpoint is only whitespace",
			settings: AppSettings{APIEndpoint: "   ", Model: "anything"},
			wantErr:  "no endpoint configured",
		},
		{
			name:     "missing model",
			settings: AppSettings{APIEndpoint: "http://example.invalid"},
			wantErr:  "no model configured",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := chatCompletion(context.Background(),tc.settings, []chatMessage{{Role: roleUser, Content: "hi"}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestChatCompletion_ResponseParsing(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		status      int
		wantText    string
		wantEmotion string
		wantNaked   bool
		wantErrSub  string
	}{
		{
			name:        "structured JSON reply",
			body:        jsonChoice(`{"text":"hello there","emotion":"amused","naked":false}`, tokenUsage{}),
			status:      200,
			wantText:    "hello there",
			wantEmotion: "amused",
		},
		{
			name:        "naked flag round-trips",
			body:        jsonChoice(`{"text":"come closer","emotion":"horny","naked":true}`, tokenUsage{}),
			status:      200,
			wantText:    "come closer",
			wantEmotion: "horny",
			wantNaked:   true,
		},
		{
			name:        "plain text fallback when JSON parse fails",
			body:        jsonChoice(`just some prose, no JSON`, tokenUsage{}),
			status:      200,
			wantText:    "just some prose, no JSON",
			wantEmotion: "neutral",
		},
		{
			name:        "missing emotion defaults to neutral",
			body:        jsonChoice(`{"text":"ok"}`, tokenUsage{}),
			status:      200,
			wantText:    "ok",
			wantEmotion: "neutral",
		},
		{
			name:        "think block stripped from content",
			body:        jsonChoice("<think>internal reasoning</think>\n"+`{"text":"final","emotion":"happy","naked":false}`, tokenUsage{}),
			status:      200,
			wantText:    "final",
			wantEmotion: "happy",
		},
		{
			name:       "server returns 500",
			body:       "boom",
			status:     500,
			wantErrSub: "HTTP 500",
		},
		{
			name:       "server returns no choices",
			body:       `{"choices":[]}`,
			status:     200,
			wantErrSub: "no choices",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := stubChatServer(t, tc.status, tc.body)
			reply, _, err := chatCompletion(context.Background(),
				AppSettings{APIEndpoint: srv.URL, Model: "m"},
				[]chatMessage{{Role: roleUser, Content: "hi"}},
			)
			if tc.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrSub)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantText, reply.Text)
			assert.Equal(t, tc.wantEmotion, reply.Emotion)
			assert.Equal(t, tc.wantNaked, reply.Naked)
		})
	}
}

func TestChatCompletion_UsageBlock(t *testing.T) {
	cases := []struct {
		name  string
		usage tokenUsage
		want  tokenUsage
	}{
		{
			name:  "all fields populated",
			usage: tokenUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
			want:  tokenUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		},
		{
			name:  "only parts reported",
			usage: tokenUsage{PromptTokens: 7, CompletionTokens: 3},
			want:  tokenUsage{PromptTokens: 7, CompletionTokens: 3},
		},
		{
			name:  "missing entirely",
			usage: tokenUsage{},
			want:  tokenUsage{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, tc.usage))
			_, got, err := chatCompletion(context.Background(),
				AppSettings{APIEndpoint: srv.URL, Model: "m"},
				[]chatMessage{{Role: roleUser, Content: "hi"}},
			)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestChatCompletion_HistoryAssembly(t *testing.T) {
	// Five user/assistant turns plus the freshly appended user turn —
	// callers always pass history with the new message at the tail.
	full := []chatMessage{
		{Role: roleUser, Content: "u1"},
		{Role: roleAssistant, Content: "a1"},
		{Role: roleUser, Content: "u2"},
		{Role: roleAssistant, Content: "a2"},
		{Role: roleUser, Content: "u3"},
	}

	cases := []struct {
		name         string
		history      int
		systemPrompt string
		wantRoles    []string // includes system if present
		wantLastUser string
	}{
		{
			name:         "no history sends only the latest user turn",
			history:      0,
			wantRoles:    []string{roleSystem, roleUser},
			wantLastUser: "u3",
		},
		{
			name:         "history cap larger than transcript sends everything",
			history:      99,
			wantRoles:    []string{roleSystem, roleUser, roleAssistant, roleUser, roleAssistant, roleUser},
			wantLastUser: "u3",
		},
		{
			name:         "history cap of 3 sends the last 3 messages",
			history:      3,
			wantRoles:    []string{roleSystem, roleUser, roleAssistant, roleUser},
			wantLastUser: "u3",
		},
		{
			name:         "system prompt is prepended",
			history:      99,
			systemPrompt: "you are diesel",
			wantRoles:    []string{roleSystem, roleSystem, roleUser, roleAssistant, roleUser, roleAssistant, roleUser},
			wantLastUser: "u3",
		},
		{
			name:         "system prompt with whitespace-only is dropped",
			history:      99,
			systemPrompt: "   \n  ",
			wantRoles:    []string{roleSystem, roleUser, roleAssistant, roleUser, roleAssistant, roleUser},
			wantLastUser: "u3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, tokenUsage{}))
			_, _, err := chatCompletion(context.Background(),
				AppSettings{
					APIEndpoint:     srv.URL,
					Model:           "m",
					HistoryMessages: tc.history,
					SystemPrompt:    tc.systemPrompt,
				},
				full,
			)
			require.NoError(t, err)

			gotRoles := make([]string, len(req.Messages))
			for i, m := range req.Messages {
				gotRoles[i] = m.Role
			}
			assert.Equal(t, tc.wantRoles, gotRoles)
			require.NotEmpty(t, req.Messages)
			last := req.Messages[len(req.Messages)-1]
			assert.Equal(t, roleUser, last.Role)
			assert.Equal(t, tc.wantLastUser, last.Content)
		})
	}
}

func TestChatCompletion_AuthHeader(t *testing.T) {
	cases := []struct {
		name       string
		apiKey     string
		wantHeader string
	}{
		{name: "key present sets bearer", apiKey: "sk-123", wantHeader: "Bearer sk-123"},
		{name: "blank key omits header", apiKey: "", wantHeader: ""},
		{name: "whitespace-only key omits header", apiKey: "   ", wantHeader: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				_, _ = io.WriteString(w, jsonChoice(`{"text":"ok","emotion":"neutral"}`, tokenUsage{}))
			}))
			t.Cleanup(srv.Close)
			_, _, err := chatCompletion(context.Background(),
				AppSettings{APIEndpoint: srv.URL, Model: "m", APIKey: tc.apiKey},
				[]chatMessage{{Role: roleUser, Content: "hi"}},
			)
			require.NoError(t, err)
			assert.Equal(t, tc.wantHeader, gotAuth)
		})
	}
}

func TestChatCompletion_RequestBodyShape(t *testing.T) {
	// Verify the request includes the structured-output schema and the
	// reasoning-disable flags for every provider variant we know about.
	// Drift here means we either lost the JSON schema constraint or
	// stopped suppressing reasoning on some provider — both are user-
	// visible regressions (verbose <think> in transcripts, free-form
	// replies that break the emotion pipeline).
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, jsonChoice(`{"text":"ok","emotion":"neutral"}`, tokenUsage{}))
	}))
	t.Cleanup(srv.Close)

	_, _, err := chatCompletion(context.Background(),
		AppSettings{APIEndpoint: srv.URL, Model: "m"},
		[]chatMessage{{Role: roleUser, Content: "hi"}},
	)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, "m", body["model"])
	assert.Equal(t, false, body["stream"])

	// Structured output constraint.
	rf, ok := body["response_format"].(map[string]any)
	require.True(t, ok, "response_format should be an object")
	assert.Equal(t, "json_schema", rf["type"])
	js, ok := rf["json_schema"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "diesel_reply", js["name"])
	assert.Equal(t, true, js["strict"])

	// The schema's emotion enum must match the in-code emotions list so
	// the model can return any emotion we know how to render.
	schema, ok := js["schema"].(map[string]any)
	require.True(t, ok)
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	emotionProp, ok := props["emotion"].(map[string]any)
	require.True(t, ok)
	enumRaw, ok := emotionProp["enum"].([]any)
	require.True(t, ok)
	gotEnum := make([]string, len(enumRaw))
	for i, v := range enumRaw {
		gotEnum[i] = v.(string)
	}
	assert.Equal(t, emotions, gotEnum)

	// Every emotion in the schema enum must have a matching prompt
	// fragment, otherwise the portrait pipeline silently renders without
	// expression steering when the model picks that emotion.
	for _, e := range emotions {
		_, ok := emotionPrompts[e]
		assert.True(t, ok, "emotionPrompts is missing an entry for %q", e)
	}

	// The naked flag must be present in properties AND required — with
	// strict mode + additionalProperties:false, OpenAI structured outputs
	// reject the schema unless every property is listed as required.
	nakedProp, ok := props["naked"].(map[string]any)
	require.True(t, ok, "naked property missing from schema")
	assert.Equal(t, "boolean", nakedProp["type"])
	requiredRaw, ok := schema["required"].([]any)
	require.True(t, ok)
	gotRequired := make([]string, len(requiredRaw))
	for i, v := range requiredRaw {
		gotRequired[i] = v.(string)
	}
	assert.ElementsMatch(t, []string{"text", "emotion", "naked"}, gotRequired)

	// Reasoning-disable: every provider variant should be set.
	assert.Equal(t, "none", body["reasoning_effort"])
	assert.Equal(t, map[string]any{"effort": "none"}, body["reasoning"])
	assert.Equal(t, map[string]any{"type": "disabled"}, body["thinking"])
	assert.Equal(t, map[string]any{"enable_thinking": false}, body["chat_template_kwargs"])
}

func TestChatCompletion_TrailingSlashEndpoint(t *testing.T) {
	// normalizeEndpoint trims trailing slashes — exercise it through the
	// chat path so a misconfigured endpoint with `/v1/` still resolves
	// the request URL to `/v1/chat/completions` rather than `//`.
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, jsonChoice(`{"text":"ok","emotion":"neutral"}`, tokenUsage{}))
	}))
	t.Cleanup(srv.Close)

	_, _, err := chatCompletion(context.Background(),
		AppSettings{APIEndpoint: srv.URL + "/", Model: "m"},
		[]chatMessage{{Role: roleUser, Content: "hi"}},
	)
	require.NoError(t, err)
	assert.Equal(t, "/chat/completions", gotPath)
	assert.False(t, strings.Contains(gotPath, "//"), "endpoint normalization should prevent double slashes")
}
