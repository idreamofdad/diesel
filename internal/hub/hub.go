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
	// EventTurnComplete fires when the LLM reply is parsed and appended
	// to history. Carries the assistant message, token usage, and (when
	// available) URLs to fetch the synthesized audio and portrait. The
	// audio URL is set only when TTS is enabled and synthesis succeeded;
	// per-client routing (last-active wins) is the subscriber's job —
	// only the subscriber whose ID matches Origin should fetch the audio.
	EventTurnComplete EventType = "turn_complete"
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
	// User and Assistant carry the messages appended to history.
	User      *chat.Message `json:"user,omitempty"`
	Assistant *chat.Message `json:"assistant,omitempty"`
	// Emotion and Naked are extracted from the structured reply.
	Emotion string `json:"emotion,omitempty"`
	Naked   bool   `json:"naked,omitempty"`
	// PortraitURL is "/api/portrait/<id>"; set on EventTurnComplete when
	// image generation produced something. Broadcast to every subscriber.
	PortraitURL string `json:"portrait_url,omitempty"`
	// AudioURL is "/api/audio/<id>"; set on EventTurnComplete when TTS
	// synthesis succeeded. Broadcast to every subscriber, but Origin
	// determines who should actually fetch+play it.
	AudioURL string      `json:"audio_url,omitempty"`
	Usage    *chat.Usage `json:"usage,omitempty"`
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

// Hub owns the conversation. Construct with New(), then call Start once
// at boot to load any persisted history and Stop at shutdown.
type Hub struct {
	mu         sync.Mutex
	history    []chat.Message
	inFlight   bool
	subs       map[string]*Subscriber
	portraits  *blobCache
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
		audio:      newBlobCache(mediaCacheSize),
		lastStatus: "Ready",
	}
}

// Start loads persisted history from disk when SaveToDisk is enabled and
// seeds the most-recent portrait into the cache so the first /api/portrait
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
		User:      &user,
		Timestamp: time.Now(),
	})
	h.setStatus("Sending…")

	go h.runTurn(ctx, s, snapshot, origin, turnID)
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
			Error:     err.Error(),
			Timestamp: time.Now(),
		})
		h.setStatus("✗ " + err.Error())
		return
	}

	assistant := chat.Message{Role: chat.RoleAssistant, Content: reply.Text, Timestamp: time.Now()}
	h.mu.Lock()
	h.history = append(h.history, assistant)
	hist := append([]chat.Message(nil), h.history...)
	h.mu.Unlock()
	if s.SaveToDisk {
		_ = conversation.Save(turnCtx, hist)
	}

	// Run TTS and portrait in parallel — both are best-effort and their
	// failure mode is "no audio / no portrait", not "turn failed".
	var (
		wg          sync.WaitGroup
		audioID     string
		portraitID  string
	)
	if s.EnableTTS && reply.Text != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ep := util.FirstNonEmpty(s.TTSEndpoint, s.APIEndpoint)
			key := util.FirstNonEmpty(s.TTSAPIKey, s.APIKey)
			data, err := tts.Synthesize(turnCtx, ep, key, s.TTSModel, s.TTSVoice, reply.Text)
			if err != nil || len(data) == 0 {
				return
			}
			id := fmt.Sprintf("%d", turnID)
			h.mu.Lock()
			h.audio.put(id, data)
			h.mu.Unlock()
			audioID = id
		}()
	}
	if s.EnableImageGen {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prompt := composeImagePrompt(s, reply.Emotion, reply.Naked)
			// onProgress callbacks are dropped — preview frames are an
			// optimization the desktop GUI used to consume. Adding them
			// back per-subscriber is a follow-up.
			png, err := comfyui.Generate(turnCtx, s, prompt, s.ImageNegativePrompt, reply.Naked, nil)
			if err != nil || len(png) == 0 {
				return
			}
			_ = comfyui.SaveCharacterImage(png)
			id := fmt.Sprintf("%d", turnID)
			h.mu.Lock()
			h.portraits.put(id, png)
			h.mu.Unlock()
			portraitID = id
		}()
	}
	wg.Wait()

	h.mu.Lock()
	h.inFlight = false
	h.mu.Unlock()

	ev := Event{
		Type:      EventTurnComplete,
		Origin:    origin,
		Assistant: &assistant,
		Emotion:   reply.Emotion,
		Naked:     reply.Naked,
		Usage:     &usage,
		Timestamp: time.Now(),
	}
	if audioID != "" {
		ev.AudioURL = "/api/audio/" + audioID
	}
	if portraitID != "" {
		ev.PortraitURL = "/api/portrait/" + portraitID
	}
	h.broadcast(ev)
	h.setStatus("Ready")
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
