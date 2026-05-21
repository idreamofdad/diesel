// Package hub is the single source of truth for a Diesel conversation.
//
// Both the Qt desktop GUI and the remote web UI talk to the same hub —
// the Qt code became a subscriber instead of owning state directly. This
// is what lets two clients see one conversation: when either side sends
// a message, the hub appends to history, runs the LLM/TTS/image
// pipelines, persists to disk, and broadcasts events to every subscriber.
//
// Concurrency model:
//   - One mutex guards history, the in-flight flag, subscriber list, and
//     the small media caches. All hub methods are safe to call from any
//     goroutine.
//   - Turns are processed strictly one at a time. A second Send() while
//     a turn is in flight returns ErrBusy; the hub broadcasts the busy
//     state so all UIs can grey out their Send buttons.
//   - Subscribers receive events on a buffered channel (drop-on-full —
//     a slow client doesn't block the hub or its peers; clients can
//     resync via History()).
//
// The hub does not import Qt. Subscribers (Qt UI, gin handlers) are
// responsible for any platform-specific side effects (audio playback,
// widget updates) — the hub only emits the data needed to do them.
package hub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"diesel/internal/chat"
	"diesel/internal/comfyui"
	"diesel/internal/conversation"
	"diesel/internal/settings"
	"diesel/internal/tracing"
	"diesel/internal/tts"
	"diesel/internal/util"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// EventType discriminates the broadcast events. Wire-stable: change with
// care — the web client switches on the string value.
type EventType string

const (
	// EventTurnStarted fires after the hub has accepted a Send() and
	// appended the user message to history. Carries the user message so
	// subscribers can append it to their transcript before the reply
	// lands. Also marks the conversation as locked for other writers.
	EventTurnStarted EventType = "turn_started"
	// EventTurnComplete fires the moment the LLM reply is parsed and
	// appended to history — before TTS synthesis or portrait rendering
	// finish. Carries the assistant message, token usage, and the
	// reply's emotion/naked flags. Audio and portrait URLs come later
	// in separate events so the transcript paints instantly instead of
	// waiting on the slowest media. inFlight clears on this event, so
	// the next turn can start while audio/portrait for this one are
	// still rendering in the background.
	EventTurnComplete EventType = "turn_complete"
	// EventAudioReady fires when TTS synthesis finishes for a turn —
	// success, failure, or skip (TTS disabled / empty reply). When
	// AudioURL is non-empty, the bytes are ready at that URL. When
	// blank, no audio will arrive for this turn — used by the desktop
	// client to advance the continuous-conversation loop without
	// waiting forever for a TTS event that's never coming.
	// Per-client routing (last-active wins) is the subscriber's job:
	// only the subscriber whose ID matches Origin should fetch+play.
	EventAudioReady EventType = "audio_ready"
	// EventPortraitReady fires when portrait rendering finishes for
	// a turn — success, failure, or skip (image gen disabled). When
	// PortraitURL is non-empty, the PNG is ready at that URL. Broadcast
	// to every subscriber.
	EventPortraitReady EventType = "portrait_ready"
	// EventPortraitProgress fires repeatedly while a portrait is
	// rendering — once per ComfyUI sampler step. When PortraitURL is
	// non-empty, an intermediate preview frame is fetchable there
	// (JPEG, typically). Step/Total carry the sampler progress; either
	// or both may be zero on frames that arrived ahead of the first
	// step event. Best-effort: subscribers whose buffers are full may
	// miss frames, which is fine — the next one (or the final
	// EventPortraitReady) will catch them up.
	EventPortraitProgress EventType = "portrait_progress"
	// EventTurnError fires when something in the pipeline failed badly
	// enough that no assistant message will arrive. Releases the lock.
	EventTurnError EventType = "turn_error"
	// EventStatus is a free-form status string suitable for a status bar.
	EventStatus EventType = "status"
	// EventCleared fires when the conversation is wiped (New Conversation).
	EventCleared EventType = "cleared"
	// EventBusy is sent only to the subscriber whose Send() was rejected
	// because another turn is in flight.
	EventBusy EventType = "busy"
)

// Event is the broadcast payload. Fields are optional and tagged for
// JSON wire use — the gin WS handler emits these directly.
type Event struct {
	Type EventType `json:"type"`
	// Origin is the ID of the subscriber that initiated the turn (set on
	// EventTurnStarted / EventTurnComplete / EventTurnError). Subscribers
	// use it to decide whether to play the reply's TTS audio locally
	// (last-active wins: only origin plays).
	Origin string `json:"origin,omitempty"`
	// TurnID is the hub's monotonic per-turn counter, set on every
	// turn-scoped event (started / complete / error, audio, portrait).
	// It lets a subscriber correlate a later media event back to the
	// turn that produced it — the Telegram bridge uses it to attach a
	// portrait to the reply it belongs to.
	TurnID int64 `json:"turn_id,omitempty"`
	// User and Assistant carry the messages appended to history.
	User      *chat.Message `json:"user,omitempty"`
	Assistant *chat.Message `json:"assistant,omitempty"`
	// Emotion and Naked are extracted from the structured reply.
	Emotion string `json:"emotion,omitempty"`
	Naked   bool   `json:"naked,omitempty"`
	// PortraitURL is "/api/v1/portrait/<id>"; set on EventTurnComplete when
	// image generation produced something. Broadcast to every subscriber.
	PortraitURL string `json:"portrait_url,omitempty"`
	// AudioURL is "/api/v1/audio/<id>"; set on EventTurnComplete when TTS
	// synthesis succeeded. Broadcast to every subscriber, but Origin
	// determines who should actually fetch+play it.
	AudioURL string      `json:"audio_url,omitempty"`
	Usage    *chat.Usage `json:"usage,omitempty"`
	// Step and Total carry sampler progress on EventPortraitProgress.
	// Either may be zero on frames that landed before the first
	// "progress" message from ComfyUI.
	Step  int `json:"step,omitempty"`
	Total int `json:"total,omitempty"`
	// Status carries a short status-bar message for EventStatus.
	Status string `json:"status,omitempty"`
	// Error is the human-readable failure message for EventTurnError.
	Error string `json:"error,omitempty"`
	// Timestamp is when the event was generated, hub-wall-clock.
	Timestamp time.Time `json:"timestamp"`
}

// ErrBusy is returned by Send when another turn is already in flight.
var ErrBusy = errors.New("hub: another turn is in progress")

// Subscriber receives broadcast events on Events. The channel is
// buffered; events that don't fit are dropped (the subscriber can
// resync by calling Hub.History()).
type Subscriber struct {
	ID     string
	Events chan Event
	closed atomic.Bool
}

// mediaCacheSize is the number of recent audio / portrait blobs the hub
// retains for HTTP fetch. Small because each one is at most ~1 MB and
// clients fetch them within seconds of the broadcast.
const mediaCacheSize = 8

// previewCacheSize bounds the in-flight portrait preview frames. One
// turn produces ~one frame per sampler step; sized to comfortably hold
// a full render or two so a slow client can still fetch the most
// recent frame after a backlog.
const previewCacheSize = 64

// telegramOriginPrefix marks turns that arrived over the Telegram bridge
// (origins look like "telegram:<chat_id>"). Those render a landscape
// portrait — Telegram displays photos wide — while every other origin
// keeps the workflow's portrait dimensions. Duplicated here as a literal
// rather than imported from internal/telegram, which imports this
// package: importing it back would create a cycle.
const telegramOriginPrefix = "telegram:"

// Hub owns the conversation. Construct with New(), then call Start once
// at boot to load any persisted history and Stop at shutdown.
type Hub struct {
	mu         sync.Mutex
	history    []chat.Message
	inFlight   bool
	subs       map[string]*Subscriber
	portraits  *blobCache
	previews   *blobCache
	audio      *blobCache
	nextTurnID int64
	// statusCh / lastStatus track the most recent status string so a
	// freshly-subscribed client can be sent the current state instead of
	// staring at a blank status bar until the next turn.
	lastStatus string
}

// New returns an empty hub. Call Start to populate from disk.
func New() *Hub {
	return &Hub{
		subs:       make(map[string]*Subscriber),
		portraits:  newBlobCache(mediaCacheSize),
		previews:   newBlobCache(previewCacheSize),
		audio:      newBlobCache(mediaCacheSize),
		lastStatus: "Ready",
	}
}

// Start loads persisted history from disk when SaveToDisk is enabled and
// seeds the most-recent portrait into the cache so the first /api/v1/portrait
// fetch hits something.
func (h *Hub) Start(ctx context.Context) {
	s := settings.Load()
	if s.SaveToDisk {
		h.mu.Lock()
		h.history = conversation.Load()
		h.mu.Unlock()
	}
	if path, err := comfyui.CharacterImagePath(); err == nil {
		if data, err := readFile(path); err == nil {
			// Seed with a deterministic ID so the initial GET works even
			// before any new portrait is rendered.
			h.mu.Lock()
			h.portraits.put("startup", data)
			h.mu.Unlock()
		}
	}
}

// Stop closes every subscriber channel. Safe to call multiple times.
func (h *Hub) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if s.closed.CompareAndSwap(false, true) {
			close(s.Events)
		}
	}
	h.subs = make(map[string]*Subscriber)
}

// Subscribe registers a new subscriber with the given stable ID. The
// returned Subscriber's Events channel is closed by Unsubscribe or
// Stop. Pass an empty id to auto-generate one.
func (h *Hub) Subscribe(id string) *Subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	if id == "" {
		id = fmt.Sprintf("sub-%d", time.Now().UnixNano())
	}
	// Replace any prior subscriber with the same ID — happens when a
	// WS client reconnects and reuses its session ID.
	if prev, ok := h.subs[id]; ok && prev.closed.CompareAndSwap(false, true) {
		close(prev.Events)
	}
	sub := &Subscriber{
		ID:     id,
		Events: make(chan Event, 64),
	}
	h.subs[id] = sub
	return sub
}

// Unsubscribe removes the subscriber and closes its channel.
func (h *Hub) Unsubscribe(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sub, ok := h.subs[id]; ok {
		delete(h.subs, id)
		if sub.closed.CompareAndSwap(false, true) {
			close(sub.Events)
		}
	}
}

// History returns a snapshot of the current history. Safe to call from
// any goroutine; the returned slice is a copy.
func (h *Hub) History() []chat.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]chat.Message, len(h.history))
	copy(out, h.history)
	return out
}

// LastStatus returns the most recently broadcast status message — used
// by freshly-connected clients to populate their status bar.
func (h *Hub) LastStatus() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastStatus
}

// InFlight reports whether a turn is currently being processed. Used by
// new subscribers to decide whether to show the busy state.
func (h *Hub) InFlight() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inFlight
}

// Portrait returns the PNG bytes for a previously-broadcast portrait ID,
// or (nil, false) if it has been evicted from the cache.
func (h *Hub) Portrait(id string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.portraits.get(id)
}

// LatestPortrait returns the most recent portrait's ID and bytes, or
// ("", nil) if none have been cached yet.
func (h *Hub) LatestPortrait() (string, []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.portraits.latest()
}

// PortraitPreview returns the bytes for a cached intermediate preview
// frame (ComfyUI emits JPEG, sometimes PNG). Previews evict quickly —
// callers should treat (nil, false) as "stale, try the latest one"
// rather than an error.
func (h *Hub) PortraitPreview(id string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.previews.get(id)
}

// Audio returns the audio bytes for a previously-broadcast audio ID.
func (h *Hub) Audio(id string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.audio.get(id)
}

// Clear wipes the conversation. Broadcasts EventCleared. Refuses while a
// turn is in flight.
func (h *Hub) Clear(ctx context.Context) error {
	h.mu.Lock()
	if h.inFlight {
		h.mu.Unlock()
		return ErrBusy
	}
	h.history = nil
	h.mu.Unlock()
	if settings.Load().SaveToDisk {
		_ = conversation.Save(ctx, nil)
	}
	h.broadcast(Event{Type: EventCleared, Timestamp: time.Now()})
	h.setStatus("Ready")
	return nil
}

// Send appends the user message and kicks off the turn pipeline in a
// goroutine. Returns ErrBusy if another turn is already in flight, in
// which case the caller's UI should stay locked until EventTurnComplete
// (or EventTurnError) arrives. `origin` identifies the subscriber that
// initiated the turn — used for last-active TTS routing and surfaced in
// the broadcast events.
func (h *Hub) Send(ctx context.Context, text, origin string) error {
	if text == "" {
		return errors.New("empty message")
	}
	h.mu.Lock()
	if h.inFlight {
		h.mu.Unlock()
		// Tell the rejected subscriber specifically — they may have
		// stale UI thinking the conversation is free.
		h.sendTo(origin, Event{Type: EventBusy, Timestamp: time.Now()})
		return ErrBusy
	}
	user := chat.Message{Role: chat.RoleUser, Content: text, Timestamp: time.Now()}
	h.history = append(h.history, user)
	h.inFlight = true
	snapshot := append([]chat.Message(nil), h.history...)
	h.nextTurnID++
	turnID := h.nextTurnID
	h.mu.Unlock()

	s := settings.Load()
	if s.SaveToDisk {
		// Best-effort — failure shouldn't block the turn.
		_ = conversation.Save(ctx, snapshot)
	}
	h.broadcast(Event{
		Type:      EventTurnStarted,
		Origin:    origin,
		TurnID:    turnID,
		User:      &user,
		Timestamp: time.Now(),
	})
	h.setStatus("Sending…")

	// Detach the goroutine's context from the caller. When Send is
	// invoked from an HTTP handler the caller's context cancels the
	// moment the handler returns — which is right after this line —
	// and that cancellation would propagate into the LLM HTTP call
	// inside runTurn, surfacing as "Post …: context canceled". The
	// turn pipeline genuinely outlives the originating request, so
	// we keep the context's values (tracing span lineage) but drop
	// the deadline/cancel.
	go h.runTurn(context.WithoutCancel(ctx), s, snapshot, origin, turnID)
	return nil
}

// runTurn executes the LLM call, then fans out TTS synthesis and image
// generation in parallel goroutines. Broadcasts EventTurnComplete once
// the LLM reply is in hand (with media URLs filled in only after the
// respective subgoroutines finish — sent as a single event so subscribers
// don't have to coalesce intermediate states). Failure of TTS or portrait
// does not fail the turn; only the chat-completion error path emits
// EventTurnError.
func (h *Hub) runTurn(ctx context.Context, s settings.AppSettings, snapshot []chat.Message, origin string, turnID int64) {
	turnCtx, turnSpan := tracing.StartSpan(ctx, "hub.turn",
		attribute.String("turn.origin", origin),
		attribute.Int64("turn.id", turnID),
	)
	defer turnSpan.End()

	reply, usage, err := chat.Completion(turnCtx, s, snapshot)
	if err != nil {
		turnSpan.RecordError(err)
		turnSpan.SetStatus(codes.Error, err.Error())
		h.mu.Lock()
		// Roll back the user turn so the next send isn't replayed with
		// a half-finished exchange in history.
		if n := len(h.history); n > 0 && h.history[n-1].Role == chat.RoleUser {
			h.history = h.history[:n-1]
		}
		h.inFlight = false
		snapshot := append([]chat.Message(nil), h.history...)
		h.mu.Unlock()
		if s.SaveToDisk {
			_ = conversation.Save(turnCtx, snapshot)
		}
		h.broadcast(Event{
			Type:      EventTurnError,
			Origin:    origin,
			TurnID:    turnID,
			Error:     err.Error(),
			Timestamp: time.Now(),
		})
		h.setStatus("✗ " + err.Error())
		return
	}

	assistant := chat.Message{Role: chat.RoleAssistant, Content: reply.Text, Emotion: reply.Emotion, Timestamp: time.Now()}
	h.mu.Lock()
	h.history = append(h.history, assistant)
	hist := append([]chat.Message(nil), h.history...)
	// Clear inFlight as soon as text lands so the user can immediately
	// send the next message — TTS and portrait rendering keep running
	// in the background and arrive on their own events.
	h.inFlight = false
	h.mu.Unlock()
	if s.SaveToDisk {
		_ = conversation.Save(turnCtx, hist)
	}

	// Text event fires immediately — no waiting on media.
	h.broadcast(Event{
		Type:      EventTurnComplete,
		Origin:    origin,
		TurnID:    turnID,
		Assistant: &assistant,
		Emotion:   reply.Emotion,
		Naked:     reply.Naked,
		Usage:     &usage,
		Timestamp: time.Now(),
	})
	h.setStatus("Ready")

	// Spawn TTS and portrait as independent goroutines. Each broadcasts
	// its own *Ready event when done — success or failure. Sentinel
	// events with empty URLs are intentional: clients (notably the
	// desktop continuous-conversation loop) need a definitive "no
	// audio for this turn" signal to advance, otherwise they'd wait
	// forever for a synthesis that's never coming.
	go h.synthesizeAudio(turnCtx, s, reply, origin, turnID)
	go h.renderPortrait(turnCtx, s, reply, origin, turnID)
}

// synthesizeAudio runs TTS and broadcasts EventAudioReady when done.
// Always broadcasts — an empty AudioURL on the event means "no audio
// for this turn", which is what subscribers need to know whether to
// keep waiting or move on.
func (h *Hub) synthesizeAudio(ctx context.Context, s settings.AppSettings, reply chat.Reply, origin string, turnID int64) {
	ev := Event{
		Type:      EventAudioReady,
		Origin:    origin,
		TurnID:    turnID,
		Timestamp: time.Now(),
	}
	defer func() {
		ev.Timestamp = time.Now()
		h.broadcast(ev)
	}()
	if !s.EnableTTS || strings.TrimSpace(reply.Text) == "" {
		return
	}
	ep := util.FirstNonEmpty(s.TTSEndpoint, s.APIEndpoint)
	key := util.FirstNonEmpty(s.TTSAPIKey, s.APIKey)
	data, err := tts.Synthesize(ctx, ep, key, s.TTSModel, s.TTSVoice, reply.Text)
	if err != nil || len(data) == 0 {
		return
	}
	id := fmt.Sprintf("%d", turnID)
	h.mu.Lock()
	h.audio.put(id, data)
	h.mu.Unlock()
	ev.AudioURL = "/api/v1/audio/" + id
}

// renderPortrait runs ComfyUI image generation and broadcasts
// EventPortraitReady when done — same always-broadcast contract as
// synthesizeAudio. Empty PortraitURL = "no portrait for this turn".
func (h *Hub) renderPortrait(ctx context.Context, s settings.AppSettings, reply chat.Reply, origin string, turnID int64) {
	ev := Event{
		Type:      EventPortraitReady,
		TurnID:    turnID,
		Timestamp: time.Now(),
	}
	defer func() {
		ev.Timestamp = time.Now()
		h.broadcast(ev)
	}()
	if !s.EnableImageGen {
		return
	}
	prompt := composeImagePrompt(s, reply.Emotion, reply.Naked)
	// Stream sampler steps and intermediate preview frames as
	// EventPortraitProgress so subscribers can paint the image as it
	// develops. Preview bytes go into the previews cache and a
	// per-frame URL rides along on the event. The callback runs on
	// the comfyui goroutine; broadcast() is safe to call there.
	var stepSeq int
	var lastStep, lastTotal int
	onProgress := func(p comfyui.Progress) {
		ev := Event{
			Type:      EventPortraitProgress,
			TurnID:    turnID,
			Timestamp: time.Now(),
		}
		if p.Total > 0 {
			lastStep = p.Step
			lastTotal = p.Total
		}
		ev.Step = lastStep
		ev.Total = lastTotal
		if len(p.Preview) > 0 {
			stepSeq++
			id := fmt.Sprintf("%d-%d", turnID, stepSeq)
			h.mu.Lock()
			h.previews.put(id, p.Preview)
			h.mu.Unlock()
			ev.PortraitURL = "/api/v1/portrait-preview/" + id
		}
		h.broadcast(ev)
	}
	landscape := strings.HasPrefix(origin, telegramOriginPrefix)
	png, err := comfyui.Generate(ctx, s, prompt, s.ImageNegativePrompt, reply.Naked, landscape, onProgress)
	if err != nil || len(png) == 0 {
		return
	}
	_ = comfyui.SaveCharacterImage(png)
	id := fmt.Sprintf("%d", turnID)
	h.mu.Lock()
	h.portraits.put(id, png)
	h.mu.Unlock()
	ev.PortraitURL = "/api/v1/portrait/" + id
}

// composeImagePrompt assembles the image prompt the same way the
// previous main.go code did — base prompt, clothing or nudity splice,
// then the emotion fragment. Kept here so the hub remains the single
// owner of the pipeline; main.go no longer needs to know the recipe.
func composeImagePrompt(s settings.AppSettings, emotion string, naked bool) string {
	prompt := trimSpace(s.ImagePrompt)
	switch {
	case naked:
		if frag := trimSpace(s.ImageNudity); frag != "" {
			prompt = prompt + ", " + frag
		}
	default:
		if frag := trimSpace(s.ImageClothing); frag != "" {
			prompt = prompt + ", " + frag
		}
	}
	if frag := chat.EmotionPrompts[trimSpace(emotion)]; frag != "" {
		prompt = prompt + ", " + frag
	}
	return prompt
}

// setStatus updates the cached status string and broadcasts it.
func (h *Hub) setStatus(msg string) {
	h.mu.Lock()
	h.lastStatus = msg
	h.mu.Unlock()
	h.broadcast(Event{Type: EventStatus, Status: msg, Timestamp: time.Now()})
}

// SetStatus is the public version — lets the gin handlers (e.g. STT
// processing) push status updates that the desktop will also display.
func (h *Hub) SetStatus(msg string) { h.setStatus(msg) }

// broadcast delivers ev to every subscriber, dropping silently on any
// subscriber whose buffer is full. Iterating under the lock is safe
// because the channel sends are non-blocking.
func (h *Hub) broadcast(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subs {
		if sub.closed.Load() {
			continue
		}
		select {
		case sub.Events <- ev:
		default:
			// Buffer full — drop. Subscriber can resync via History().
		}
	}
}

// sendTo delivers ev to a single subscriber by ID, if present.
func (h *Hub) sendTo(id string, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub, ok := h.subs[id]
	if !ok || sub.closed.Load() {
		return
	}
	select {
	case sub.Events <- ev:
	default:
	}
}
