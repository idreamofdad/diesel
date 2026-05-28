package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"time"

	"diesel/internal/settings"
	"diesel/internal/tracing"
	"diesel/internal/util"

	qt "github.com/mappu/miqt/qt6"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// AppendTurn writes a "<who>: <body>" paragraph to the transcript with the
// speaker label rendered in `color`. Both args are HTML-escaped before
// formatting and newlines in the body become <br> so multi-line replies
// keep their line breaks — QTextEdit interprets Append() content as rich
// text by default, which means unescaped user content would otherwise be
// parsed (and characters like `<` would vanish).
//
// We move the cursor to the end and ensure it's visible after appending.
// QTextEdit.Append only scrolls when the cursor was already at the end
// before the append, so without this the view stays pinned to whatever
// the user last clicked and the latest turn slides off the bottom.
func AppendTurn(transcript *qt.QTextEdit, who, body, color string) {
	safeBody := strings.ReplaceAll(html.EscapeString(body), "\n", "<br>")
	transcript.Append(fmt.Sprintf(
		`<span style="color:%s;"><b>%s:</b></span> %s`,
		color, html.EscapeString(who), safeBody,
	))
	transcript.MoveCursor(qt.QTextCursor__End)
	transcript.EnsureCursorVisible()
}

// Chat message roles, matching the OpenAI-compatible /chat/completions
// schema. Defined as constants so the spellings live in one place.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is the wire shape for an OpenAI-compatible /chat/completions
// turn. We also keep a slice of these in memory (and on disk) as the
// conversation log, stamped with the wall-clock time the turn occurred so
// the model can reason about elapsed time. Timestamp and Emotion are
// bookkeeping fields: Timestamp is folded into Content before each
// request, and both are zeroed on the outgoing copy, so the wire body
// stays a plain role/content pair.
type Message struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp,omitempty"`
	// Emotion is the expression the model chose for an assistant turn
	// (one of Emotions). Stored on the message so the next request can
	// remind the model of its previous expression — see lastEmotion.
	// Empty on user/system messages and on assistant turns from older
	// conversation files saved before this field existed.
	Emotion string `json:"emotion,omitempty"`
	// Naked is the nudity flag the model raised on an assistant turn.
	// Stored alongside Emotion so the next request can remind the model
	// of its previous state of dress — see lastNaked. Always false on
	// user/system messages and on assistant turns from older conversation
	// files saved before this field existed.
	Naked bool `json:"naked,omitempty"`
}

// thinkBlock matches the <think>…</think> reasoning blocks some OSS models
// (Qwen3, DeepSeek-R1 distills, …) emit inline in the assistant content,
// even when we ask them not to. We strip those so the transcript only
// shows the final answer.
var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

// leadingTimestamp matches a `[YYYY-MM-DD HH:MM:SS]` or `[HH:MM:SS]` prefix
// (optionally with a timezone abbreviation) that models sometimes echo at
// the start of their reply because we stamp user turns that way before
// sending them. The date portion is optional so we also catch the short
// `[06:58:56]` form some models truncate to.
var leadingTimestamp = regexp.MustCompile(`^\s*\[(?:\d{4}-\d{2}-\d{2} )?\d{2}:\d{2}:\d{2}(?:\s+\S+)?\]\s*`)

// Usage mirrors the `usage` block OpenAI-compatible servers return on
// /chat/completions. All fields are optional — local servers (LM Studio,
// llama.cpp, …) sometimes omit it or report 0 — so callers should treat
// zero values as "unknown" and not as "definitely no tokens".
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Reply is the structured shape Diesel asks the model to return on every
// turn. The text goes to the transcript and TTS; the emotion drives the
// portrait pipeline (it's appended as an expression to the image prompt).
// Naked is a per-turn nudity flag the model can raise when it thinks the
// scene calls for it — the portrait pipeline splices a nudity fragment
// into the image prompt when true. The JSON tags match the response_format
// schema below — don't rename either side in isolation.
type Reply struct {
	Text    string `json:"text"`
	Emotion string `json:"emotion"`
	Naked   bool   `json:"naked"`
}

// Emotions is the closed set the model is constrained to choose from.
// Each entry must have a matching key in comfyui.EmotionPrompts so the
// portrait pipeline knows how to render it.
var Emotions = []string{
	"happy", "sad", "angry", "surprised happy", "surprised shocked", "laughing",
	"neutral", "amused", "annoyed", "thoughtful", "flirtatious", "horny",
}

// lastEmotion returns the Emotion of the most recent assistant message
// in `history`, or "" when the conversation has no assistant turn yet
// (or it predates the Emotion field). Used to feed the model its own
// previous expression for portrait continuity.
func lastEmotion(history []Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == RoleAssistant {
			return history[i].Emotion
		}
	}
	return ""
}

// lastNaked returns the Naked flag of the most recent assistant message in
// `history`. The second return is false when the conversation has no
// assistant turn yet, so the caller can tell "clothed" apart from "no prior
// turn". Used to feed the model its own previous state of dress.
func lastNaked(history []Message) (bool, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == RoleAssistant {
			return history[i].Naked, true
		}
	}
	return false, false
}

// Completion sends `history` (oldest→newest) to the configured endpoint
// and returns the assistant's structured reply along with the server-
// reported token usage (zero-valued struct when the server didn't include
// one). Reasoning/"thinking" is explicitly disabled via every shape we
// know of — extra fields a server doesn't understand are ignored by
// OpenAI-compatible implementations.
//
// The request asks for a strict JSON schema (Reply); LM Studio and OpenAI
// honor it. Providers that ignore response_format will return plain text,
// which we treat as the whole `text` with a neutral emotion so the
// conversation keeps flowing rather than erroring out.
func Completion(ctx context.Context, s settings.AppSettings, history []Message) (Reply, Usage, error) {
	ctx, span := tracing.StartSpan(ctx, "llm.chat",
		attribute.String("llm.model", s.Model),
		attribute.Int("llm.history.messages", len(history)),
		attribute.Bool("llm.system_prompt", strings.TrimSpace(s.SystemPrompt) != ""),
	)
	defer span.End()

	endpoint := util.NormalizeEndpoint(s.APIEndpoint)
	if endpoint == "" {
		err := fmt.Errorf("no endpoint configured")
		span.SetStatus(codes.Error, err.Error())
		return Reply{}, Usage{}, err
	}
	if strings.TrimSpace(s.Model) == "" {
		err := fmt.Errorf("no model configured")
		span.SetStatus(codes.Error, err.Error())
		return Reply{}, Usage{}, err
	}

	// Assemble the outgoing message list: optional system prompt, then the
	// trailing window of history capped at HistoryMessages turns. The
	// caller has already appended the latest user message to `history`.
	msgs := make([]Message, 0, len(history)+2)
	msgs = append(msgs, Message{
		Role:    RoleSystem,
		Content: "Current date and time: " + time.Now().Format("Monday, January 2, 2006 at 3:04 PM MST"),
	})
	if sp := strings.TrimSpace(s.SystemPrompt); sp != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: sp})
	}
	// Remind the model of the expression it last wore so the portrait
	// emotion has some turn-to-turn continuity. Skipped on the first turn
	// of a conversation, where there's no prior assistant reply.
	if e := lastEmotion(history); e != "" {
		msgs = append(msgs, Message{
			Role:    RoleSystem,
			Content: "Your facial expression in your previous reply was: " + e,
		})
	}
	// Likewise remind the model of its previous state of dress so the
	// nudity flag has turn-to-turn continuity. Skipped on the first turn,
	// where there's no prior assistant reply.
	if naked, ok := lastNaked(history); ok {
		state := "clothed"
		if naked {
			state = "nude"
		}
		msgs = append(msgs, Message{
			Role:    RoleSystem,
			Content: "Your state of dress in your previous reply was: " + state,
		})
	}
	start := 0
	switch {
	case s.HistoryMessages <= 0:
		// "No history" still has to include the current user turn.
		start = len(history) - 1
	case len(history) > s.HistoryMessages:
		start = len(history) - s.HistoryMessages
	}
	if start < 0 {
		start = 0
	}
	for _, m := range history[start:] {
		if !m.Timestamp.IsZero() {
			m.Content = "[" + m.Timestamp.Format("2006-01-02 15:04:05 MST") + "] " + m.Content
			m.Timestamp = time.Time{}
		}
		// Emotion and Naked are internal bookkeeping — strip them so the
		// wire body stays a plain role/content pair. The model's prior
		// expression and state of dress are fed back via the system
		// messages above, not on the turn.
		m.Emotion = ""
		m.Naked = false
		msgs = append(msgs, m)
	}

	body := map[string]any{
		"model":    s.Model,
		"messages": msgs,
		"stream":   false,
		// Constrain the reply to {text, emotion} via OpenAI-compatible
		// structured outputs. LM Studio and OpenAI honor this strictly;
		// providers that ignore response_format fall through to the
		// plain-text fallback in the response parser below.
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "diesel_reply",
				"strict": true,
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{"type": "string"},
						"emotion": map[string]any{
							"type": "string",
							"enum": Emotions,
						},
						"naked": map[string]any{"type": "boolean"},
					},
					"required":             []string{"text", "emotion", "naked"},
					"additionalProperties": false,
				},
			},
		},
		// Disable reasoning across the providers we might be talking to:
		//   • OpenAI reasoning models   → reasoning_effort
		//   • Anthropic extended thinking → thinking.type
		//   • Qwen3 / DeepSeek via llama.cpp/vLLM/LM Studio → chat_template_kwargs
		"reasoning_effort":     "none",
		"reasoning":            map[string]any{"effort": "none"},
		"thinking":             map[string]any{"type": "disabled"},
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Reply{}, Usage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Reply{}, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(s.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	// Long ceiling: local servers running a big model on a laptop can take
	// well over a minute for the first token. We don't stream yet, so the
	// whole completion has to fit inside this timeout.
	client := tracing.HTTPClient(5 * time.Minute)
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Reply{}, Usage{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		err := util.HTTPStatusError(resp, 512)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Reply{}, Usage{}, err
	}

	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Reply{}, Usage{}, err
	}
	// Attach the server-reported usage even on the fallback path below —
	// callers (and span consumers) want the token counts regardless of
	// whether the reply parsed as structured JSON.
	span.SetAttributes(
		attribute.Int("llm.usage.prompt_tokens", payload.Usage.PromptTokens),
		attribute.Int("llm.usage.completion_tokens", payload.Usage.CompletionTokens),
		attribute.Int("llm.usage.total_tokens", payload.Usage.TotalTokens),
	)
	if len(payload.Choices) == 0 {
		err := fmt.Errorf("server returned no choices")
		span.SetStatus(codes.Error, err.Error())
		return Reply{}, Usage{}, err
	}
	content := strings.TrimSpace(thinkBlock.ReplaceAllString(payload.Choices[0].Message.Content, ""))
	content = leadingTimestamp.ReplaceAllString(content, "")

	// Parse the structured reply. The schema is strict, so a healthy LM
	// Studio / OpenAI response is valid JSON — trust it whenever it
	// unmarshals, even when text is empty: an empty-text reply means the
	// model legitimately chose to say nothing, and treating that as a
	// parse failure would dump the raw `{"text":"",...}` blob straight
	// into the transcript. Only a genuine unmarshal error (provider
	// ignored response_format and returned prose) falls back to raw
	// content with a neutral emotion so the chat keeps flowing.
	var parsed Reply
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		span.SetAttributes(
			attribute.Bool("llm.structured_reply", false),
			attribute.Int("llm.reply.length", len(content)),
			attribute.String("llm.reply.emotion", "neutral"),
		)
		return Reply{Text: content, Emotion: "neutral"}, payload.Usage, nil
	}
	parsed.Text = leadingTimestamp.ReplaceAllString(parsed.Text, "")
	if parsed.Emotion == "" {
		parsed.Emotion = "neutral"
	}
	span.SetAttributes(
		attribute.Bool("llm.structured_reply", true),
		attribute.Int("llm.reply.length", len(parsed.Text)),
		attribute.String("llm.reply.emotion", parsed.Emotion),
		attribute.Bool("llm.reply.naked", parsed.Naked),
	)
	return parsed, payload.Usage, nil
}
