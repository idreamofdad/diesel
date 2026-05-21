package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"diesel/internal/settings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chatRequest is what we expect to receive at the /chat/completions
// endpoint — used to assert the marshaled body matches the configured
// history window and system prompt.
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
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
func jsonChoice(content string, usage Usage) string {
	envelope := map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{"content": content},
		}},
		"usage": usage,
	}
	out, _ := json.Marshal(envelope)
	return string(out)
}

func TestCompletion_ConfigurationErrors(t *testing.T) {
	cases := []struct {
		name     string
		settings settings.AppSettings
		wantErr  string
	}{
		{
			name:     "missing endpoint",
			settings: settings.AppSettings{Model: "anything"},
			wantErr:  "no endpoint configured",
		},
		{
			name:     "endpoint is only whitespace",
			settings: settings.AppSettings{APIEndpoint: "   ", Model: "anything"},
			wantErr:  "no endpoint configured",
		},
		{
			name:     "missing model",
			settings: settings.AppSettings{APIEndpoint: "http://example.invalid"},
			wantErr:  "no model configured",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Completion(context.Background(), tc.settings, []Message{{Role: RoleUser, Content: "hi"}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestCompletion_ResponseParsing(t *testing.T) {
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
			body:        jsonChoice(`{"text":"hello there","emotion":"amused","naked":false}`, Usage{}),
			status:      200,
			wantText:    "hello there",
			wantEmotion: "amused",
		},
		{
			name:        "naked flag round-trips",
			body:        jsonChoice(`{"text":"come closer","emotion":"horny","naked":true}`, Usage{}),
			status:      200,
			wantText:    "come closer",
			wantEmotion: "horny",
			wantNaked:   true,
		},
		{
			name:        "plain text fallback when JSON parse fails",
			body:        jsonChoice(`just some prose, no JSON`, Usage{}),
			status:      200,
			wantText:    "just some prose, no JSON",
			wantEmotion: "neutral",
		},
		{
			// A valid structured reply with empty text must NOT be
			// mistaken for a parse failure — otherwise the raw JSON blob
			// leaks into the transcript.
			name:        "empty text in a valid structured reply",
			body:        jsonChoice(`{"text":"","emotion":"amused","naked":true}`, Usage{}),
			status:      200,
			wantText:    "",
			wantEmotion: "amused",
			wantNaked:   true,
		},
		{
			name:        "missing emotion defaults to neutral",
			body:        jsonChoice(`{"text":"ok"}`, Usage{}),
			status:      200,
			wantText:    "ok",
			wantEmotion: "neutral",
		},
		{
			name:        "think block stripped from content",
			body:        jsonChoice("<think>internal reasoning</think>\n"+`{"text":"final","emotion":"happy","naked":false}`, Usage{}),
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
			reply, _, err := Completion(context.Background(),
				settings.AppSettings{APIEndpoint: srv.URL, Model: "m"},
				[]Message{{Role: RoleUser, Content: "hi"}},
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

func TestCompletion_UsageBlock(t *testing.T) {
	cases := []struct {
		name  string
		usage Usage
		want  Usage
	}{
		{
			name:  "all fields populated",
			usage: Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
			want:  Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		},
		{
			name:  "only parts reported",
			usage: Usage{PromptTokens: 7, CompletionTokens: 3},
			want:  Usage{PromptTokens: 7, CompletionTokens: 3},
		},
		{
			name:  "missing entirely",
			usage: Usage{},
			want:  Usage{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, tc.usage))
			_, got, err := Completion(context.Background(),
				settings.AppSettings{APIEndpoint: srv.URL, Model: "m"},
				[]Message{{Role: RoleUser, Content: "hi"}},
			)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCompletion_HistoryAssembly(t *testing.T) {
	// Five user/assistant turns plus the freshly appended user turn —
	// callers always pass history with the new message at the tail.
	full := []Message{
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: "a1"},
		{Role: RoleUser, Content: "u2"},
		{Role: RoleAssistant, Content: "a2"},
		{Role: RoleUser, Content: "u3"},
	}

	cases := []struct {
		name         string
		history      int
		systemPrompt string
		wantRoles    []string // includes system if present
		wantLastUser string
	}{
		// The trailing RoleSystem after the date (and optional prompt) is
		// the state-of-dress reminder — `full` has assistant turns, so
		// lastNaked always emits it.
		{
			name:         "no history sends only the latest user turn",
			history:      0,
			wantRoles:    []string{RoleSystem, RoleSystem, RoleUser},
			wantLastUser: "u3",
		},
		{
			name:         "history cap larger than transcript sends everything",
			history:      99,
			wantRoles:    []string{RoleSystem, RoleSystem, RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser},
			wantLastUser: "u3",
		},
		{
			name:         "history cap of 3 sends the last 3 messages",
			history:      3,
			wantRoles:    []string{RoleSystem, RoleSystem, RoleUser, RoleAssistant, RoleUser},
			wantLastUser: "u3",
		},
		{
			name:         "system prompt is prepended",
			history:      99,
			systemPrompt: "you are diesel",
			wantRoles:    []string{RoleSystem, RoleSystem, RoleSystem, RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser},
			wantLastUser: "u3",
		},
		{
			name:         "system prompt with whitespace-only is dropped",
			history:      99,
			systemPrompt: "   \n  ",
			wantRoles:    []string{RoleSystem, RoleSystem, RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser},
			wantLastUser: "u3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
			_, _, err := Completion(context.Background(),
				settings.AppSettings{
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
			assert.Equal(t, RoleUser, last.Role)
			assert.Equal(t, tc.wantLastUser, last.Content)
		})
	}
}

func TestCompletion_LastEmotionSystemMessage(t *testing.T) {
	t.Run("prior assistant emotion is fed back as a system message", func(t *testing.T) {
		srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
		_, _, err := Completion(context.Background(),
			settings.AppSettings{APIEndpoint: srv.URL, Model: "m", HistoryMessages: 99},
			[]Message{
				{Role: RoleUser, Content: "u1"},
				{Role: RoleAssistant, Content: "a1", Emotion: "amused"},
				{Role: RoleUser, Content: "u2"},
			},
		)
		require.NoError(t, err)
		var found bool
		for _, m := range req.Messages {
			if m.Role == RoleSystem && strings.Contains(m.Content, "amused") {
				found = true
			}
		}
		assert.True(t, found, "expected a system message naming the last emotion")
	})

	t.Run("most recent assistant emotion wins", func(t *testing.T) {
		srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
		_, _, err := Completion(context.Background(),
			settings.AppSettings{APIEndpoint: srv.URL, Model: "m", HistoryMessages: 99},
			[]Message{
				{Role: RoleAssistant, Content: "a1", Emotion: "happy"},
				{Role: RoleUser, Content: "u1"},
				{Role: RoleAssistant, Content: "a2", Emotion: "annoyed"},
				{Role: RoleUser, Content: "u2"},
			},
		)
		require.NoError(t, err)
		var emotionMsgs []string
		for _, m := range req.Messages {
			if m.Role == RoleSystem && strings.Contains(m.Content, "facial expression") {
				emotionMsgs = append(emotionMsgs, m.Content)
			}
		}
		require.Len(t, emotionMsgs, 1)
		assert.Contains(t, emotionMsgs[0], "annoyed")
		assert.NotContains(t, emotionMsgs[0], "happy")
	})

	t.Run("no assistant turn yet means no emotion system message", func(t *testing.T) {
		srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
		_, _, err := Completion(context.Background(),
			settings.AppSettings{APIEndpoint: srv.URL, Model: "m"},
			[]Message{{Role: RoleUser, Content: "hi"}},
		)
		require.NoError(t, err)
		for _, m := range req.Messages {
			assert.NotContains(t, m.Content, "facial expression")
		}
	})

	t.Run("emotion is stripped from the assistant turn on the wire", func(t *testing.T) {
		srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
		_, _, err := Completion(context.Background(),
			settings.AppSettings{APIEndpoint: srv.URL, Model: "m", HistoryMessages: 99},
			[]Message{
				{Role: RoleUser, Content: "u1"},
				{Role: RoleAssistant, Content: "a1", Emotion: "amused"},
				{Role: RoleUser, Content: "u2"},
			},
		)
		require.NoError(t, err)
		for _, m := range req.Messages {
			if m.Role == RoleAssistant {
				assert.Empty(t, m.Emotion, "emotion must not ride on the wire assistant turn")
			}
		}
	})

	t.Run("prior assistant nudity state is fed back as a system message", func(t *testing.T) {
		srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
		_, _, err := Completion(context.Background(),
			settings.AppSettings{APIEndpoint: srv.URL, Model: "m", HistoryMessages: 99},
			[]Message{
				{Role: RoleUser, Content: "u1"},
				{Role: RoleAssistant, Content: "a1", Naked: true},
				{Role: RoleUser, Content: "u2"},
			},
		)
		require.NoError(t, err)
		var found bool
		for _, m := range req.Messages {
			if m.Role == RoleSystem && strings.Contains(m.Content, "state of dress") {
				found = true
				assert.Contains(t, m.Content, "nude")
			}
		}
		assert.True(t, found, "expected a system message naming the last state of dress")
	})

	t.Run("clothed prior turn reports clothed, not nude", func(t *testing.T) {
		srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
		_, _, err := Completion(context.Background(),
			settings.AppSettings{APIEndpoint: srv.URL, Model: "m", HistoryMessages: 99},
			[]Message{
				{Role: RoleUser, Content: "u1"},
				{Role: RoleAssistant, Content: "a1", Naked: false},
				{Role: RoleUser, Content: "u2"},
			},
		)
		require.NoError(t, err)
		var dressMsgs []string
		for _, m := range req.Messages {
			if m.Role == RoleSystem && strings.Contains(m.Content, "state of dress") {
				dressMsgs = append(dressMsgs, m.Content)
			}
		}
		require.Len(t, dressMsgs, 1)
		assert.Contains(t, dressMsgs[0], "clothed")
		assert.NotContains(t, dressMsgs[0], "nude")
	})

	t.Run("no assistant turn yet means no state-of-dress system message", func(t *testing.T) {
		srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
		_, _, err := Completion(context.Background(),
			settings.AppSettings{APIEndpoint: srv.URL, Model: "m"},
			[]Message{{Role: RoleUser, Content: "hi"}},
		)
		require.NoError(t, err)
		for _, m := range req.Messages {
			assert.NotContains(t, m.Content, "state of dress")
		}
	})

	t.Run("naked flag is stripped from the assistant turn on the wire", func(t *testing.T) {
		srv, req := stubChatServer(t, 200, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
		_, _, err := Completion(context.Background(),
			settings.AppSettings{APIEndpoint: srv.URL, Model: "m", HistoryMessages: 99},
			[]Message{
				{Role: RoleUser, Content: "u1"},
				{Role: RoleAssistant, Content: "a1", Naked: true},
				{Role: RoleUser, Content: "u2"},
			},
		)
		require.NoError(t, err)
		for _, m := range req.Messages {
			if m.Role == RoleAssistant {
				assert.False(t, m.Naked, "naked flag must not ride on the wire assistant turn")
			}
		}
	})
}

func TestCompletion_AuthHeader(t *testing.T) {
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
				_, _ = io.WriteString(w, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
			}))
			t.Cleanup(srv.Close)
			_, _, err := Completion(context.Background(),
				settings.AppSettings{APIEndpoint: srv.URL, Model: "m", APIKey: tc.apiKey},
				[]Message{{Role: RoleUser, Content: "hi"}},
			)
			require.NoError(t, err)
			assert.Equal(t, tc.wantHeader, gotAuth)
		})
	}
}

func TestCompletion_RequestBodyShape(t *testing.T) {
	// Verify the request includes the structured-output schema and the
	// reasoning-disable flags for every provider variant we know about.
	// Drift here means we either lost the JSON schema constraint or
	// stopped suppressing reasoning on some provider — both are user-
	// visible regressions (verbose <think> in transcripts, free-form
	// replies that break the emotion pipeline).
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
	}))
	t.Cleanup(srv.Close)

	_, _, err := Completion(context.Background(),
		settings.AppSettings{APIEndpoint: srv.URL, Model: "m"},
		[]Message{{Role: RoleUser, Content: "hi"}},
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

	// The schema's emotion enum must match the in-code Emotions list so
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
	assert.Equal(t, Emotions, gotEnum)

	// Every emotion in the schema enum must have a matching prompt
	// fragment, otherwise the portrait pipeline silently renders without
	// expression steering when the model picks that emotion.
	for _, e := range Emotions {
		_, ok := EmotionPrompts[e]
		assert.True(t, ok, "EmotionPrompts is missing an entry for %q", e)
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

func TestCompletion_TrailingSlashEndpoint(t *testing.T) {
	// util.NormalizeEndpoint trims trailing slashes — exercise it through
	// the chat path so a misconfigured endpoint with `/v1/` still resolves
	// the request URL to `/v1/chat/completions` rather than `//`.
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, jsonChoice(`{"text":"ok","emotion":"neutral"}`, Usage{}))
	}))
	t.Cleanup(srv.Close)

	_, _, err := Completion(context.Background(),
		settings.AppSettings{APIEndpoint: srv.URL + "/", Model: "m"},
		[]Message{{Role: RoleUser, Content: "hi"}},
	)
	require.NoError(t, err)
	assert.Equal(t, "/chat/completions", gotPath)
	assert.False(t, strings.Contains(gotPath, "//"), "endpoint normalization should prevent double slashes")
}
