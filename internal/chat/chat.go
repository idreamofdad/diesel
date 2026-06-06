package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"diesel/internal/comfyui"
	"diesel/internal/logging"
	"diesel/internal/settings"
	"diesel/internal/tracing"
	"diesel/internal/util"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Chat message roles, matching the OpenAI-compatible /chat/completions
// schema. Defined as constants so the spellings live in one place.
var logger = logging.Component("chat")

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	// RoleTool labels a message carrying a tool's result back to the model,
	// keyed to the assistant's tool_call by ToolCallID. Only used on the
	// knowledge-graph tool-calling path.
	RoleTool = "tool"
)

// maxMemoryRounds bounds how many rounds of write-tool calls one memory pass
// may make before we stop it. A confused model that keeps calling tools can't
// loop forever or rack up unbounded background work; after this many rounds we
// give up on the pass. (The reply path advertises no tools, so this cap is the
// memory pass's alone.)
const maxMemoryRounds = 10

// knowledgeTokenWarn is the rough token size of the injected graph blob past
// which we log a warning. The graph is persistent memory and only grows, so
// this is an early smoke alarm for "the system prompt is getting big" — not a
// truncation point (we still inject the whole thing).
const knowledgeTokenWarn = 6000

// Message is the wire shape for an OpenAI-compatible /chat/completions
// turn. We also keep a slice of these in memory (and on disk) as the
// conversation log, stamped with the wall-clock time the turn occurred so
// the model can reason about elapsed time. Timestamp, Emotion, Naked,
// Background, and Pose are bookkeeping fields: Timestamp is folded into
// Content before each request, and all are zeroed on the outgoing copy,
// so the wire body stays a plain role/content pair.
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
	// Background is the scene slug the model chose for an assistant turn
	// (one of Backgrounds). Stored so the next request can remind the
	// model of where Diesel was last shown — see lastBackground. Empty
	// on user/system messages and on older saved assistant turns.
	Background string `json:"background,omitempty"`
	// Pose is the body posture slug the model chose for an assistant
	// turn (one of Poses). Stored so the next request can remind the
	// model of Diesel's last posture — see lastPose. Empty on
	// user/system messages and on older saved assistant turns.
	Pose string `json:"pose,omitempty"`
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
// into the image prompt when true. Background and Pose pick the scene
// and posture the portrait pipeline composes around; both are constrained
// to closed enums (Backgrounds, Poses) so a misspelling can't slip past
// the matrix lookup. The JSON tags match the response_format schema
// below — don't rename either side in isolation.
type Reply struct {
	Text       string `json:"text"`
	Emotion    string `json:"emotion"`
	Naked      bool   `json:"naked"`
	Background string `json:"background"`
	Pose       string `json:"pose"`
}

// Emotions is the closed set the model is constrained to choose from.
// Each entry must have a matching key in comfyui.EmotionPrompts so the
// portrait pipeline knows how to render it.
var Emotions = []string{
	"happy", "sad", "angry", "surprised happy", "surprised shocked", "laughing",
	"neutral", "amused", "annoyed", "thoughtful", "flirtatious", "horny",
}

// Backgrounds is the closed set of scene slugs the model can choose from.
// Each entry must have a matching key in comfyui.ImageBackgrounds so the
// portrait pipeline knows how to render it; chat_test guards the
// correspondence.
var Backgrounds = []string{
	"living_room", "mechanics_shop", "forest_park", "pub",
}

// Poses is the closed set of body-posture slugs the model can choose
// from. Each entry must have a matching key in comfyui.ImagePoseBases
// AND a row in comfyui.ImagePoseAddons populated for every background;
// chat_test guards both.
var Poses = []string{
	"standing", "sitting", "bent_over",
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

// lastBackground returns the Background slug of the most recent assistant
// message in `history`, or "" when the conversation has no assistant turn
// yet (or it predates the Background field). Used both to remind the model
// of the prior scene and to inherit on the structured-reply fallback path.
func lastBackground(history []Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == RoleAssistant {
			return history[i].Background
		}
	}
	return ""
}

// lastPose mirrors lastBackground for the body-posture field.
func lastPose(history []Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == RoleAssistant {
			return history[i].Pose
		}
	}
	return ""
}

// wireMsg is the request-side shape of a /chat/completions message. It's a
// superset of the plain role/content pair: ToolCalls rides on an assistant
// turn that wants to call tools, and ToolCallID labels a RoleTool message
// carrying a tool's result back. Kept separate from Message (the history /
// persisted type) so the tool-calling machinery never leaks into the
// conversation log — tool turns are transient request scaffolding, not
// transcript.
type wireMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// toolCall mirrors the OpenAI tool_call object: an id the tool result must
// echo back, a type (always "function"), and the function name + raw JSON
// argument string the model produced.
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Completion sends `history` (oldest→newest) to the configured endpoint and
// returns the assistant's structured reply with the server-reported token
// usage (zero-valued when the server omits it).
//
// When kb is non-nil, Diesel's persistent memory is in play: the whole
// knowledge graph is injected into the system prompt as JSON, and the graph's
// MCP tools are advertised so the model can update its memory mid-turn. The
// model runs through a tool-call loop and then produces the strict Reply
// schema. When no KnowledgeBase is supplied (or a nil one), this is a single
// structured-output call with reasoning disabled — the original, tool-free
// behavior — so existing callers and tests are unaffected. The trailing
// parameter is optional purely so those call sites stay terse; callers pass at
// most one.
func Completion(ctx context.Context, s settings.AppSettings, history []Message, kbs ...KnowledgeBase) (Reply, Usage, error) {
	var kb KnowledgeBase
	if len(kbs) > 0 {
		kb = kbs[0]
	}
	ctx, span := tracing.StartSpan(ctx, "llm.chat",
		attribute.String("llm.model", s.Model),
		attribute.Int("llm.history.messages", len(history)),
		attribute.Bool("llm.knowledge", kb != nil),
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

	// Reply path: graph injected for reading (when kb is set), strict schema,
	// reasoning disabled, NO tools. Keeping tools off the reply request is what
	// makes this robust — combining tools with a strict response_format makes
	// many local models either 400 or silently refuse to ever call a tool.
	// Memory writes happen separately in MemoryPass, after the reply is sent.
	msgs := assembleMessages(ctx, s, history, kb)
	choice, usage, _, err := callLLM(ctx, s, endpoint, buildBody(s.Model, msgs, nil, true, true))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Reply{}, usage, err
	}
	recordUsage(span, usage)
	return finalizeReply(choice.Content, history, span), usage, nil
}

// memoryHistoryMessages caps how many recent messages the memory pass reviews.
// It's deliberately independent of s.HistoryMessages (which can run to hundreds
// of turns for the chat itself): the extractor only needs the recent tail to
// pull durable facts from, and a fixed, modest window keeps the pass cheap.
const memoryHistoryMessages = 20

// memoryInstruction steers the second-pass model: look at the conversation it
// just had and record anything durable through the write tools. It's carried in
// the memory pass's single user turn, right after the transcript it refers to
// (the graph rides separately as system context). It's deliberately concrete
// and example-driven — small local models follow a worked example far more
// reliably than abstract rules.
const memoryInstruction = `# Updating your memory

You are now updating your own long-term memory based on the conversation above. This is a background step — do NOT write a chat reply, do NOT explain or reason out loud, just call the tools. Go straight to the right tool calls; the worked example below is all the guidance you need.

Your memory is a knowledge graph of three things:
- ENTITIES — the people, animals, places, and things you know. Each has a unique name and a type (e.g. name "{first_name} {last_name}", type "person"; name "{pet_name}", type "cat").
- OBSERVATIONS — short factual notes attached to one entity (e.g. on "{first_name} {last_name}": "works at McDonalds").
- RELATIONS — directed links between two entities, written in active voice (e.g. "{first_name} {last_name}" —owns→ "{pet_name}").

You have these tools:
- create_entities — add new entities. Pass each as {name, entityType, observations:[]}. Re-using a name just merges in new observations, so it's safe.
- add_observations — attach new facts to an entity that already exists. Pass {entityName, contents:[...]}.
- create_relations — link two entities that BOTH already exist. Pass {from, to, relationType}. If an endpoint doesn't exist yet, create it first or the call is rejected.
- delete_entities / delete_observations / delete_relations — remove things that are now wrong, irrelevant, or contradicted.

Worked example — if the user said "I'm {first_name} {last_name}, I work at McDonalds, and I have a cat named {pet_name}", you would call:
1. create_entities: [{name:"{first_name} {last_name}", entityType:"person", observations:["works at McDonalds"]}, {name:"{pet_name}", entityType:"cat", observations:[]}]
2. create_relations: [{from:"{first_name} {last_name}", to:"{pet_name}", relationType:"owns"}]

Counter-example — if the user said "ugh, rough day at work, I'm wiped, gonna crash early" and the graph already knows {first_name} works at McDonalds, you call NOTHING. The bad mood and being tired are passing, and the job is already on record. Calling no tools is the most common correct outcome — most turns add nothing durable.

Rules:
- Don't think step by step or narrate what you're doing — read the conversation, then act with tool calls directly.
- Default to doing nothing. Recording too much is worse than recording too little: clutter buries the facts that matter. Only reach for a tool when a clearly durable, genuinely new fact appeared.
- DURABLE means it would still be true and worth knowing weeks from now: names, jobs, relationships, pets, where someone lives, lasting preferences. NOT durable, so ignore it entirely: moods, what someone did or felt today, plans for tonight, small talk, one-off events, opinions said in passing.
- Check the graph above before writing anything. If a fact is already there — even worded differently — do nothing. Don't re-create an entity that exists, don't re-add an observation that's present, and don't add a near-duplicate (e.g. "has a cat" when "owns {pet_name}, a cat" is already recorded).
- You already know everything in your background above — your own life and work, your relationship with {first_name} {last_name}, the people and pets it describes. None of that gets recorded; it's part of who you are, not something you just learned. Only record NEW facts {first_name} gives you that go beyond both your background and the graph.
- Create entities before relating them.
- If a new fact genuinely contradicts an old one, delete the stale piece and add the correct one.
- If nothing new and durable came up — the common case — call no tools at all.

Understandings:
- {first_name} may call you "dad" or "daddy" — that's the role you've grown into, not a literal family relationship.
`

// MemoryPass is the second pass of the turn: after the user already has their
// reply, the model gets the conversation plus its current memory and a set of
// write-only tools, and records anything durable it learned. It is tools-only
// (no response_format), so the tools+schema incompatibility never arises — the
// model is free to call tools with nothing competing for the output. Runs to a
// natural stop (the model emits no more tool calls) or the iteration cap.
// Best-effort: any error is returned for logging but doesn't affect the reply,
// which has already been delivered. A nil kb or empty tool set is a no-op.
func MemoryPass(ctx context.Context, s settings.AppSettings, history []Message, kb KnowledgeBase) error {
	if kb == nil {
		return nil
	}
	// The memory pass is the only tool-calling path, so it runs on the
	// (optionally separate) tool model. ResolveToolModel applies the blank
	// fall-through to the main model, so this is the main model unless the user
	// configured a distinct one. We override s with the resolved endpoint/key/
	// model so callLLM's auth header and buildBody's model both pick it up.
	toolEndpoint, toolKey, toolModel := s.ResolveToolModel()
	s.APIEndpoint, s.APIKey, s.Model = toolEndpoint, toolKey, toolModel

	ctx, span := tracing.StartSpan(ctx, "llm.memory",
		attribute.String("llm.model", toolModel),
	)
	defer span.End()

	endpoint := util.NormalizeEndpoint(toolEndpoint)
	if endpoint == "" || strings.TrimSpace(toolModel) == "" {
		return fmt.Errorf("memory: no endpoint or model configured")
	}

	defs, err := kb.Tools(ctx)
	if err != nil {
		return fmt.Errorf("memory: list tools: %w", err)
	}
	tools := toolsToWire(defs)
	if len(tools) == 0 {
		logger.Debug().Msg("memory pass: no write tools advertised, skipping")
		return nil
	}

	msgs := memoryMessages(ctx, s, history, kb)

	rounds := 0
	for iter := 0; iter < maxMemoryRounds; iter++ {
		// A newer turn supersedes this pass by cancelling ctx — stop cleanly
		// between rounds rather than firing another doomed request. This is the
		// "reset": the next turn's pass starts the round count over from zero.
		if ctx.Err() != nil {
			logger.Debug().Msgf("memory pass superseded after %d round(s)", rounds)
			return nil
		}
		// Tools-only, no schema, reasoning DISABLED. A thinking model left to
		// reason here tends to spend the turn emitting a <think> block and
		// return no tool calls at all; with thinking off it goes straight to
		// calling the write tools, which the worked example primes it for.
		body := buildBody(s.Model, msgs, tools, false, true)
		logger.Debug().Msgf("memory round %d request: tools=%d messages=%d", iter, len(tools), len(msgs))
		choice, _, status, err := callLLM(ctx, s, endpoint, body)
		if err != nil {
			// A cancel mid-request surfaces as a transport error; that's the
			// supersede path, not a real failure, so don't log it as one.
			if ctx.Err() != nil {
				logger.Debug().Msgf("memory pass superseded mid-request after %d round(s)", rounds)
				return nil
			}
			logger.Debug().Err(err).Msgf("memory round %d error: status=%d", iter, status)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
		logger.Debug().Msgf("memory round %d response: toolCalls=%d content=%q", iter, len(choice.ToolCalls), truncate(choice.Content, 400))
		if len(choice.ToolCalls) == 0 {
			logger.Debug().Msgf("memory pass complete after %d round(s) of tool calls", rounds)
			span.SetAttributes(attribute.Int("llm.memory.rounds", rounds))
			return nil
		}
		rounds++
		msgs = append(msgs, wireMsg{Role: RoleAssistant, Content: choice.Content, ToolCalls: choice.ToolCalls})
		for _, tc := range choice.ToolCalls {
			logger.Debug().Msgf("memory → tool call %q args=%s", tc.Function.Name, truncate(tc.Function.Arguments, 300))
			result, callErr := kb.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			if callErr != nil {
				logger.Debug().Err(callErr).Msgf("memory ← tool %q TRANSPORT ERROR", tc.Function.Name)
				result = "Error: " + callErr.Error()
			} else {
				logger.Debug().Msgf("memory ← tool %q result=%s", tc.Function.Name, truncate(result, 300))
			}
			msgs = append(msgs, wireMsg{Role: RoleTool, ToolCallID: tc.ID, Content: result})
		}
	}
	logger.Debug().Msgf("memory pass hit the %d-round cap", maxMemoryRounds)
	span.SetAttributes(attribute.Int("llm.memory.rounds", rounds))
	return nil
}

// memoryMessages builds the focused prompt for the memory pass: the persona, the
// current memory graph, a plain transcript of the recent exchange, and the
// how-to instruction. The persona rides first, but framed as reference context
// ("this is what you already know — don't record it"), NOT as a directive to
// converse. It earns its place by killing duplicates: without it the model can't
// tell which facts are already baked into who Diesel is (the user's job, the
// cat, the relationship) and re-records them as observations every turn. The
// framing prefix and the memoryInstruction both stress this step is tool-calls
// only, to keep the persona's "reply in 1–3 sentences, stay in character" pull
// from turning the pass back into a chat reply.
func memoryMessages(ctx context.Context, s settings.AppSettings, history []Message, kb KnowledgeBase) []wireMsg {
	msgs := make([]wireMsg, 0, 4)

	// The persona is reference, not a role to perform: the label leads with
	// "you're not chatting right now" precisely because the persona itself says
	// to converse, and says nothing here should ever be recorded.
	msgs = append(msgs, wireMsg{
		Role: RoleSystem,
		Content: settings.ApplyNames(s, "For reference only — this is who you are, the background you already know by heart. "+
			"You are NOT being asked to chat or stay in character right now; your only job this step is to "+
			"call the memory tools (or none). Nothing in this background should ever be recorded as memory — "+
			"it's already part of you. It's here so you can tell what you already know apart from anything "+
			"genuinely new {first_name} {last_name} said:\n\n") + settings.RenderSystemPrompt(s),
	})

	if graph, err := kb.GraphJSON(ctx); err == nil && graph != "" {
		msgs = append(msgs, wireMsg{
			Role:    RoleSystem,
			Content: "Your current memory, as a knowledge graph in JSON:\n\n" + graph,
		})
	}

	// Render the recent turns as a plain transcript so the model treats them as
	// material to extract from, not a conversation to continue. The user's lines
	// carry their real name (matching their graph entity) rather than a generic
	// "User:", so the model attaches facts to the right entity without guessing.
	userLabel := strings.TrimSpace(s.FirstName + " " + s.LastName)
	if userLabel == "" {
		userLabel = "User"
	}
	userLabel += ": "
	start := 0
	if len(history) > memoryHistoryMessages {
		start = len(history) - memoryHistoryMessages
	}
	var b strings.Builder
	for _, m := range history[start:] {
		switch m.Role {
		case RoleUser:
			b.WriteString(userLabel)
		case RoleAssistant:
			b.WriteString("Diesel: ")
		default:
			continue
		}
		b.WriteString(strings.TrimSpace(m.Content))
		b.WriteByte('\n')
	}

	// The transcript-to-review and the how-to instruction ride together as the
	// single USER turn. Everything else here is system context (the persona and
	// the graph), and a request with no user message at all makes strict chat
	// templates 400 with "No user query found in messages" — which is exactly what
	// a separately configured tools model with a stricter template does. Folding
	// them into one user turn also leaves a user message last, which templates
	// expect before they generate. ApplyNames swaps the {first_name}/{last_name}
	// placeholders in the instruction for the configured names.
	msgs = append(msgs, wireMsg{
		Role: RoleUser,
		Content: "Conversation to review (most recent last):\n\n" + b.String() +
			"\n\n" + settings.ApplyNames(s, memoryInstruction),
	})
	return msgs
}

// assembleMessages builds the outgoing message list: the date and persona
// system messages, the injected knowledge graph (when kb is set), the
// emotion/dress/scene/pose continuity reminders, and the trailing history
// window capped at HistoryMessages turns. The caller has already appended the
// latest user message to history.
func assembleMessages(ctx context.Context, s settings.AppSettings, history []Message, kb KnowledgeBase) []wireMsg {
	msgs := make([]wireMsg, 0, len(history)+4)
	msgs = append(msgs, wireMsg{
		Role:    RoleSystem,
		Content: "Current date and time: " + time.Now().Format("Monday, January 2, 2006 at 3:04 PM MST"),
	})
	msgs = append(msgs, wireMsg{Role: RoleSystem, Content: settings.RenderSystemPrompt(s)})

	// Inject the knowledge graph right after the persona so the model reads
	// "who Diesel is" then "what Diesel remembers" before anything else. The
	// blob only grows; warn (don't truncate) once it gets large.
	if kb != nil {
		if graph, err := kb.GraphJSON(ctx); err != nil {
			logger.Debug().Err(err).Msg("knowledge graph unavailable for injection")
		} else if graph != "" {
			logger.Debug().Msgf("injecting knowledge graph: %d bytes (~%d tokens)", len(graph), settings.EstimateTokens(graph))
			if n := settings.EstimateTokens(graph); n > knowledgeTokenWarn {
				logger.Warn().Msgf("knowledge graph is large (~%d tokens); consider pruning", n)
			}
			msgs = append(msgs, wireMsg{
				Role: RoleSystem,
				Content: "This is your persistent memory — everything you currently know about the people, " +
					"animals, places, and relationships in your life, as a knowledge graph in JSON. Treat it as " +
					"established fact and stay consistent with it: use it to remember names, who owns what, where " +
					"people work, and so on. Don't recite it back or mention that you have a \"knowledge graph\" — " +
					"just let it inform how you talk, the way real memory does.\n\n" + graph,
			})
		}
	}

	if e := lastEmotion(history); e != "" {
		msgs = append(msgs, wireMsg{Role: RoleSystem, Content: "Your facial expression in your previous reply was: " + e})
	}
	if naked, ok := lastNaked(history); ok {
		state := "clothed"
		if naked {
			state = "nude"
		}
		msgs = append(msgs, wireMsg{Role: RoleSystem, Content: "Your state of dress in your previous reply was: " + state})
	}
	if bg := lastBackground(history); bg != "" {
		if spec, ok := comfyui.ImageBackgrounds[bg]; ok {
			msgs = append(msgs, wireMsg{Role: RoleSystem, Content: "You were last shown in: " + spec.Label})
		}
	}
	if p := lastPose(history); p != "" {
		if spec, ok := comfyui.ImagePoseBases[p]; ok {
			msgs = append(msgs, wireMsg{Role: RoleSystem, Content: "Your last pose was: " + spec.Label})
		}
	}

	start := 0
	switch {
	case s.HistoryMessages <= 0:
		start = len(history) - 1
	case len(history) > s.HistoryMessages:
		start = len(history) - s.HistoryMessages
	}
	if start < 0 {
		start = 0
	}
	for _, m := range history[start:] {
		content := m.Content
		if !m.Timestamp.IsZero() {
			content = "[" + m.Timestamp.Format("2006-01-02 15:04:05 MST") + "] " + content
		}
		// Emotion/Naked/Background/Pose are internal bookkeeping fed back via
		// the system messages above, not on the turn.
		msgs = append(msgs, wireMsg{Role: m.Role, Content: content})
	}
	return msgs
}

// buildBody assembles the /chat/completions request body. tools (when
// non-empty) advertises the knowledge functions with tool_choice "auto";
// withSchema attaches the strict Reply response_format; disableReasoning sends
// the family of "no thinking" flags. Unknown fields are ignored by
// OpenAI-compatible servers.
func buildBody(model string, msgs []wireMsg, tools []map[string]any, withSchema, disableReasoning bool) map[string]any {
	body := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   false,
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	if withSchema {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "diesel_reply",
				"strict": true,
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text":       map[string]any{"type": "string"},
						"emotion":    map[string]any{"type": "string", "enum": Emotions},
						"naked":      map[string]any{"type": "boolean"},
						"background": map[string]any{"type": "string", "enum": Backgrounds},
						"pose":       map[string]any{"type": "string", "enum": Poses},
					},
					"required":             []string{"text", "emotion", "naked", "background", "pose"},
					"additionalProperties": false,
				},
			},
		}
	}
	if disableReasoning {
		// Disable reasoning across the providers we might talk to:
		//   • OpenAI reasoning models     → reasoning_effort
		//   • Anthropic extended thinking → thinking.type
		//   • Qwen3 / DeepSeek via llama.cpp/vLLM/LM Studio → chat_template_kwargs
		body["reasoning_effort"] = "none"
		body["reasoning"] = map[string]any{"effort": "none"}
		body["thinking"] = map[string]any{"type": "disabled"}
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	return body
}

// toolsToWire converts the knowledge ToolDefs into the OpenAI tools array
// shape: {type:"function", function:{name, description, parameters:<schema>}}.
func toolsToWire(defs []ToolDef) []map[string]any {
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		// Start from a minimal valid object schema and overlay the tool's own.
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		if len(d.Schema) > 0 {
			var parsed map[string]any
			if err := json.Unmarshal(d.Schema, &parsed); err != nil {
				logger.Warn().Err(err).Msgf("skipping tool %q with bad schema", d.Name)
				continue
			}
			params = parsed
			// A no-arg tool (e.g. read_graph) infers to {"type":"object"} with
			// no "properties" key. Some OpenAI-compatible backends (LM Studio)
			// strictly require function.parameters.properties to be present and
			// 400 the whole request when it's missing — backfill it so every
			// advertised tool carries a valid object schema.
			if _, ok := params["type"]; !ok {
				params["type"] = "object"
			}
			if _, ok := params["properties"]; !ok {
				params["properties"] = map[string]any{}
			}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        d.Name,
				"description": d.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

// truncate shortens s to at most n runes for log output, appending an ellipsis
// marker when it had to cut. Keeps debug lines readable when a tool result or
// argument blob is large.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("…(+%d)", len(r)-n)
}

// llmChoice is the assistant message we care about from a completion: its text
// content and any tool calls it requested.
type llmChoice struct {
	Content   string
	ToolCalls []toolCall
}

// callLLM POSTs one request body and returns the assistant choice, the server-
// reported usage, the HTTP status, and an error. The status is returned even
// on error so the caller can decide whether a failure looks like a
// tools/schema incompatibility worth retrying in two-phase mode.
func callLLM(ctx context.Context, s settings.AppSettings, endpoint string, body map[string]any) (llmChoice, Usage, int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return llmChoice{}, Usage{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return llmChoice{}, Usage{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(s.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// Long ceiling: a big local model can take over a minute, and a turn may
	// make several calls. We don't stream yet, so each completion must fit.
	_, hasTools := body["tools"]
	_, hasSchema := body["response_format"]
	_, reasoningOff := body["reasoning_effort"]
	logger.Debug().Msgf("POST %s/chat/completions (tools=%v schema=%v reasoningDisabled=%v bodyBytes=%d)",
		endpoint, hasTools, hasSchema, reasoningOff, len(raw))
	client := tracing.HTTPClient(5 * time.Minute)
	resp, err := client.Do(req)
	if err != nil {
		logger.Debug().Err(err).Msg("transport error")
		return llmChoice{}, Usage{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		bodyErr := util.HTTPStatusError(resp, 512)
		logger.Debug().Msgf("non-200: status=%d body=%v", resp.StatusCode, bodyErr)
		return llmChoice{}, Usage{}, resp.StatusCode, bodyErr
	}

	var payload struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return llmChoice{}, Usage{}, resp.StatusCode, err
	}
	if len(payload.Choices) == 0 {
		return llmChoice{}, payload.Usage, resp.StatusCode, fmt.Errorf("server returned no choices")
	}
	return llmChoice{
		Content:   payload.Choices[0].Message.Content,
		ToolCalls: payload.Choices[0].Message.ToolCalls,
	}, payload.Usage, resp.StatusCode, nil
}

// finalizeReply parses content as the strict Reply JSON, applying the same
// fallbacks the tool-free path always used: a non-JSON body becomes plain text
// with a neutral emotion, and missing scene/pose inherit from the last
// assistant turn (then the hardcoded defaults) so the portrait pipeline always
// has a valid slug.
func finalizeReply(content string, history []Message, span trace.Span) Reply {
	content = strings.TrimSpace(thinkBlock.ReplaceAllString(content, ""))
	content = leadingTimestamp.ReplaceAllString(content, "")

	var parsed Reply
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		bg := lastBackground(history)
		if bg == "" {
			bg = comfyui.DefaultImageBackground
		}
		pose := lastPose(history)
		if pose == "" {
			pose = comfyui.DefaultImagePose
		}
		span.SetAttributes(
			attribute.Bool("llm.structured_reply", false),
			attribute.Int("llm.reply.length", len(content)),
			attribute.String("llm.reply.emotion", "neutral"),
			attribute.String("llm.reply.background", bg),
			attribute.String("llm.reply.pose", pose),
		)
		return Reply{Text: content, Emotion: "neutral", Background: bg, Pose: pose}
	}
	parsed.Text = leadingTimestamp.ReplaceAllString(parsed.Text, "")
	if parsed.Emotion == "" {
		parsed.Emotion = "neutral"
	}
	if parsed.Background == "" {
		parsed.Background = lastBackground(history)
		if parsed.Background == "" {
			parsed.Background = comfyui.DefaultImageBackground
		}
	}
	if parsed.Pose == "" {
		parsed.Pose = lastPose(history)
		if parsed.Pose == "" {
			parsed.Pose = comfyui.DefaultImagePose
		}
	}
	span.SetAttributes(
		attribute.Bool("llm.structured_reply", true),
		attribute.Int("llm.reply.length", len(parsed.Text)),
		attribute.String("llm.reply.emotion", parsed.Emotion),
		attribute.Bool("llm.reply.naked", parsed.Naked),
		attribute.String("llm.reply.background", parsed.Background),
		attribute.String("llm.reply.pose", parsed.Pose),
	)
	return parsed
}

// addUsage sums two usage blocks across the multiple LLM calls a tool-using
// turn can make, so the caller sees the turn's full token cost.
func addUsage(a, b Usage) Usage {
	return Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
	}
}

// recordUsage stamps the (possibly summed) token usage onto the span.
func recordUsage(span trace.Span, u Usage) {
	span.SetAttributes(
		attribute.Int("llm.usage.prompt_tokens", u.PromptTokens),
		attribute.Int("llm.usage.completion_tokens", u.CompletionTokens),
		attribute.Int("llm.usage.total_tokens", u.TotalTokens),
	)
}
