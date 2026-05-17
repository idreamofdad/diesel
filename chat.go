package main

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

	qt "github.com/mappu/miqt/qt6"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// appendTurn writes a "<who>: <body>" paragraph to the transcript with the
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
func appendTurn(transcript *qt.QTextEdit, who, body, color string) {
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
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
)

// chatMessage is the wire shape for an OpenAI-compatible /chat/completions
// turn. We also keep a slice of these in memory as the conversation log,
// stamped with the wall-clock time the turn occurred so the model can
// reason about elapsed time. Timestamp is folded into Content before each
// request and zeroed on the outgoing copy, so the wire body stays a plain
// role/content pair.
type chatMessage struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp,omitempty"`
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

// tokenUsage mirrors the `usage` block OpenAI-compatible servers return on
// /chat/completions. All fields are optional — local servers (LM Studio,
// llama.cpp, …) sometimes omit it or report 0 — so callers should treat
// zero values as "unknown" and not as "definitely no tokens".
type tokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatReply is the structured shape Diesel asks the model to return on
// every turn. The text goes to the transcript and TTS; the emotion drives
// the portrait pipeline (it's appended as an expression to the image
// prompt). Naked is a per-turn nudity flag the model can raise when it
// thinks the scene calls for it — the portrait pipeline splices a nudity
// fragment into the image prompt when true. The JSON tags match the
// response_format schema below — don't rename either side in isolation.
type chatReply struct {
	Text    string `json:"text"`
	Emotion string `json:"emotion"`
	Naked   bool   `json:"naked"`
}

// emotions is the closed set the model is constrained to choose from. Each
// entry must have a matching key in emotionPrompts so the portrait pipeline
// knows how to render it.
var emotions = []string{
	"happy", "sad", "angry", "surprised happy", "surprised shocked", "laughing",
	"neutral", "amused", "annoyed", "thoughtful", "flirtatious", "horny",
}

// emotionPrompts maps each emotion to the prompt fragment spliced into the
// image prompt to steer the portrait's expression. Values are tuned as
// SD-style comma-separated tag lists rather than bare adjectives so the
// renderer has concrete features to latch onto (mouth shape, eye state,
// brow position). An empty value (neutral) skips the splice and renders
// the base prompt unchanged.
var emotionPrompts = map[string]string{
	"happy":             "warm smile, bright eyes, cheerful expression",
	"sad":               "downturned mouth, sorrowful eyes, slight tear, melancholy expression",
	"angry":             "furrowed brow, scowl, gritted teeth, angry expression",
	"surprised happy":   "wide delighted eyes, open smiling mouth, raised eyebrows, pleasantly surprised expression",
	"surprised shocked": "wide shocked eyes, mouth agape, raised eyebrows, alarmed expression",
	"laughing":          "head tilted back, mouth wide open laughing, squinted eyes, joyful laughter",
	"neutral":           "",
	"amused":            "subtle smirk, raised eyebrow, glint in the eyes, amused expression",
	"annoyed":           "narrowed eyes, slight frown, pursed lips, annoyed expression",
	"thoughtful":        "hand on chin, distant gaze, slightly furrowed brow, contemplative expression",
	"flirtatious":       "half-lidded eyes, playful smirk, raised eyebrow, flirtatious expression",
	"horny":             "flushed cheeks, half-lidded eyes, parted lips, biting lower lip, aroused expression, smirk",
}

// nudityPrompt is the default fragment spliced into the image prompt when
// the structured reply's Naked flag is true. The active value lives in
// AppSettings.ImageNudityPrompt so the user can retune it from Settings;
// this constant is just the seed for a fresh install.
const nudityPrompt = "completely nude, naked, no clothing"

// chatCompletion sends `history` (oldest→newest) to the configured endpoint
// and returns the assistant's structured reply along with the server-
// reported token usage (zero-valued struct when the server didn't include
// one). Reasoning/"thinking" is explicitly disabled via every shape we
// know of — extra fields a server doesn't understand are ignored by
// OpenAI-compatible implementations.
//
// The request asks for a strict JSON schema (chatReply); LM Studio and
// OpenAI honor it. Providers that ignore response_format will return plain
// text, which we treat as the whole `text` with a neutral emotion so the
// conversation keeps flowing rather than erroring out.
func chatCompletion(ctx context.Context, s AppSettings, history []chatMessage) (chatReply, tokenUsage, error) {
	ctx, span := startSpan(ctx, "llm.chat",
		attribute.String("llm.model", s.Model),
		attribute.Int("llm.history.messages", len(history)),
		attribute.Bool("llm.system_prompt", strings.TrimSpace(s.SystemPrompt) != ""),
	)
	defer span.End()

	endpoint := normalizeEndpoint(s.APIEndpoint)
	if endpoint == "" {
		err := fmt.Errorf("no endpoint configured")
		span.SetStatus(codes.Error, err.Error())
		return chatReply{}, tokenUsage{}, err
	}
	if strings.TrimSpace(s.Model) == "" {
		err := fmt.Errorf("no model configured")
		span.SetStatus(codes.Error, err.Error())
		return chatReply{}, tokenUsage{}, err
	}

	// Assemble the outgoing message list: optional system prompt, then the
	// trailing window of history capped at HistoryMessages turns. The
	// caller has already appended the latest user message to `history`.
	msgs := make([]chatMessage, 0, len(history)+2)
	msgs = append(msgs, chatMessage{
		Role:    roleSystem,
		Content: "Current date and time: " + time.Now().Format("Monday, January 2, 2006 at 3:04 PM MST"),
	})
	if sp := strings.TrimSpace(s.SystemPrompt); sp != "" {
		msgs = append(msgs, chatMessage{Role: roleSystem, Content: sp})
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
							"enum": emotions,
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
		return chatReply{}, tokenUsage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return chatReply{}, tokenUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(s.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	// Long ceiling: local servers running a big model on a laptop can take
	// well over a minute for the first token. We don't stream yet, so the
	// whole completion has to fit inside this timeout.
	client := tracedHTTPClient(5 * time.Minute)
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return chatReply{}, tokenUsage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err := httpStatusError(resp, 512)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return chatReply{}, tokenUsage{}, err
	}

	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage tokenUsage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return chatReply{}, tokenUsage{}, err
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
		return chatReply{}, tokenUsage{}, err
	}
	content := strings.TrimSpace(thinkBlock.ReplaceAllString(payload.Choices[0].Message.Content, ""))
	content = leadingTimestamp.ReplaceAllString(content, "")

	// Parse the structured reply. The schema is strict, so a healthy LM
	// Studio / OpenAI response is valid JSON; on anything else (provider
	// ignored response_format, model refused, content includes prose
	// around the JSON) we surface the raw text with a neutral emotion so
	// the chat keeps flowing.
	var parsed chatReply
	if err := json.Unmarshal([]byte(content), &parsed); err != nil || parsed.Text == "" {
		span.SetAttributes(
			attribute.Bool("llm.structured_reply", false),
			attribute.Int("llm.reply.length", len(content)),
			attribute.String("llm.reply.emotion", "neutral"),
		)
		return chatReply{Text: content, Emotion: "neutral"}, payload.Usage, nil
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
