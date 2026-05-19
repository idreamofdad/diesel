package sms

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"diesel/internal/hub"
	"diesel/internal/settings"
	"diesel/internal/tracing"
)

// subscriberID is the stable hub-subscriber ID the manager registers under.
// Origin strings on the hub.Send call are formatted "sms:+E164" so the
// dispatch loop can ignore non-SMS turns and the outbound reply knows
// which number to text back.
const subscriberID = "sms"

// originPrefix tags every Send origin so subscribers (us, downstream
// listeners, the audio routing logic) can distinguish SMS-originated
// turns from desktop/web ones.
const originPrefix = "sms:"

// defaultPollSeconds is used when the settings field is unset or
// non-positive. Twilio's rate limits are generous; 10 s is fast enough
// that texts feel responsive without burning the per-second budget.
const defaultPollSeconds = 10

// minPollSeconds clamps absurdly small intervals — a user typo of "1"
// would hammer the API and risk a 429.
const minPollSeconds = 3

// Manager owns the SMS poll loop, the hub subscription, and the
// dispatch goroutine that fans assistant replies back out as SMS. The
// shape mirrors server.Manager so the desktop bootstraps the same way:
// New(hub), then Apply(settings) at startup and on every save.
type Manager struct {
	hub *hub.Hub

	mu       sync.Mutex
	applied  config
	cancel   context.CancelFunc
	status   string
	// lastSender is set by the poll loop to the most-recent inbound
	// number and read by the dispatch loop to address the outbound
	// reply. Guarded by mu because both goroutines touch it. Cleared
	// after a successful outbound or a turn error so a stale value
	// doesn't accidentally text a previous sender on a desktop turn.
	lastSender string
}

// config is the subset of AppSettings the manager cares about — used
// for change detection in Apply so a no-op save doesn't bounce the loop.
type config struct {
	enabled  bool
	sid      string
	token    string
	from     string
	allowed  []string // already-normalized to E.164
	pollSecs int
}

// configFor extracts the SMS-relevant fields, normalizes the allow-list,
// and clamps the poll interval into a reasonable range.
func configFor(s settings.AppSettings) config {
	allowed := make([]string, 0, len(s.SMSAllowedNumbers))
	for _, n := range s.SMSAllowedNumbers {
		if v := strings.TrimSpace(n); v != "" {
			allowed = append(allowed, v)
		}
	}
	poll := s.SMSPollSeconds
	if poll <= 0 {
		poll = defaultPollSeconds
	}
	if poll < minPollSeconds {
		poll = minPollSeconds
	}
	return config{
		enabled:  s.EnableSMS,
		sid:      strings.TrimSpace(s.TwilioAccountSID),
		token:    strings.TrimSpace(s.TwilioAuthToken),
		from:     strings.TrimSpace(s.TwilioFromNumber),
		allowed:  allowed,
		pollSecs: poll,
	}
}

// equal is a structural compare used by Apply to short-circuit a no-op
// re-apply. allowed is a slice so we have to walk it.
func (c config) equal(o config) bool {
	if c.enabled != o.enabled || c.sid != o.sid || c.token != o.token ||
		c.from != o.from || c.pollSecs != o.pollSecs ||
		len(c.allowed) != len(o.allowed) {
		return false
	}
	for i := range c.allowed {
		if c.allowed[i] != o.allowed[i] {
			return false
		}
	}
	return true
}

// validate returns an error explaining what's missing — used so the
// status string in the Settings dialog tells the user exactly which
// field still needs filling in.
func (c config) validate() error {
	switch {
	case c.sid == "":
		return errors.New("Account SID is empty")
	case c.token == "":
		return errors.New("Auth Token is empty")
	case c.from == "":
		return errors.New("From number is empty")
	case len(c.allowed) == 0:
		return errors.New("no allowed numbers configured")
	}
	return nil
}

// isAllowed reports whether `from` matches an entry in the allow list.
// Comparison is case-insensitive on the trimmed value so a user who
// types "+1 (202) 555-0100" still authorizes "+12025550100" — we strip
// spaces, dashes, and parentheses from both sides before comparing.
func (c config) isAllowed(from string) bool {
	want := normalizeNumber(from)
	for _, n := range c.allowed {
		if normalizeNumber(n) == want {
			return true
		}
	}
	return false
}

// normalizeNumber strips formatting characters so phone numbers compare
// equally regardless of how they were typed. The output is suitable for
// equality but not for display.
func normalizeNumber(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '-', '(', ')', '.', '\t':
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// New returns a stopped Manager bound to the given hub. Apply must be
// called to start it.
func New(h *hub.Hub) *Manager {
	return &Manager{
		hub:    h,
		status: "○ Stopped",
	}
}

// Status returns the human-readable state for the Settings dialog.
func (m *Manager) Status() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Apply brings the manager in line with the given settings. Idempotent:
// re-applying the same config is a no-op. On a config change the prior
// poll + dispatch goroutines are cancelled before new ones start so we
// never run two pollers at once.
func (m *Manager) Apply(s settings.AppSettings) string {
	cfg := configFor(s)

	m.mu.Lock()
	if cfg.equal(m.applied) && m.cancel != nil {
		st := m.status
		m.mu.Unlock()
		return st
	}
	// Tear down any prior loop. Done under the lock so a second
	// concurrent Apply can't race to start two pollers.
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
		m.hub.Unsubscribe(subscriberID)
	}
	m.lastSender = ""
	m.applied = cfg

	if !cfg.enabled {
		m.status = "○ Stopped"
		st := m.status
		m.mu.Unlock()
		return st
	}
	if err := cfg.validate(); err != nil {
		m.status = "✗ " + err.Error()
		st := m.status
		m.mu.Unlock()
		return st
	}

	// Hub subscription registered before any goroutine starts so the
	// first poll-triggered turn definitely sees its TurnComplete event.
	sub := m.hub.Subscribe(subscriberID)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	client := &Client{AccountSID: cfg.sid, AuthToken: cfg.token}
	m.status = "● Running — polling every " + fmt.Sprintf("%ds", cfg.pollSecs)
	st := m.status
	m.mu.Unlock()

	go m.pollLoop(ctx, client, cfg)
	go m.dispatchLoop(ctx, sub, client, cfg)
	return st
}

// Stop shuts the manager down. Safe at any time, even when already stopped.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
		m.hub.Unsubscribe(subscriberID)
	}
	m.status = "○ Stopped"
	m.applied = config{}
	m.lastSender = ""
}

// pollLoop hits Twilio every cfg.pollSecs and pushes new inbound
// messages into the hub. Dedup is by SID — we keep a bounded set of
// recently-seen IDs (persisted to disk across restarts) so a message
// isn't reprocessed if the app bounces between the time Twilio sent
// it and the time we acked it via the assistant reply.
func (m *Manager) pollLoop(ctx context.Context, client *Client, cfg config) {
	state := loadPollState()
	// Fresh install or a wiped state file: seed the cursor at "now" so
	// we don't replay the entire Twilio inbox the first time the app
	// runs. A normal restart finds a populated cursor here and resumes
	// from it, so messages that arrived while the app was down are
	// still picked up.
	if state.Cursor.IsZero() {
		state.Cursor = time.Now().UTC().Add(-time.Second)
	}
	seen := newSeenSet(200)
	for _, sid := range state.SeenSIDs {
		seen.add(sid)
	}

	tick := time.NewTicker(time.Duration(cfg.pollSecs) * time.Second)
	defer tick.Stop()

	// Fire one poll immediately on startup so a message that landed
	// during the brief window before we subscribed isn't delayed by a
	// full interval. The ticker handles every poll after that.
	m.pollOnce(ctx, client, cfg, &state.Cursor, seen)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.pollOnce(ctx, client, cfg, &state.Cursor, seen)
		}
	}
}

// pollOnce performs a single list call and dispatches new inbound
// messages. `since` is updated to the largest DateSent observed so the
// next poll asks for a tighter window. State is flushed to disk
// whenever new SIDs were added so a restart doesn't reprocess them.
func (m *Manager) pollOnce(ctx context.Context, client *Client, cfg config, since *time.Time, seen *seenSet) {
	ctx, span := tracing.StartSpan(ctx, "sms.poll")
	defer span.End()

	// Subtract a small skew tolerance so a message dated exactly on the
	// cursor boundary still shows up. Dedup by SID makes the overlap safe.
	queryAt := since.Add(-2 * time.Second)
	msgs, err := client.ListInbound(ctx, cfg.from, queryAt)
	if err != nil {
		// Don't tear the loop down on a transient error — Twilio
		// occasionally returns 503 and the next tick recovers. Surface
		// via status so the user notices a persistent failure.
		m.setStatus("✗ Twilio poll: " + err.Error())
		log.Printf("[sms] poll error: %v", err)
		return
	}
	// Restore the steady-state status if a prior error left it set.
	m.setStatus("● Running — polling every " + fmt.Sprintf("%ds", cfg.pollSecs))

	for _, msg := range msgs {
		if msg.SID == "" || seen.has(msg.SID) {
			continue
		}
		seen.add(msg.SID)
		// Advance the cursor to the largest DateSent we've seen so the
		// next poll asks for a tighter window. Done before the allow-list
		// check so a non-allowed sender's timestamp still advances us
		// — otherwise a stream of dropped messages would peg the cursor.
		if t := msg.ParsedDateSent(); !t.IsZero() && t.After(*since) {
			*since = t
		}
		// Persist the seen-state BEFORE we hand the message to the hub.
		// A crash between hub.Send and the next poll would otherwise
		// replay this message on restart — worse than the rare case
		// where we miss a reply (the user notices and retries).
		if err := savePollState(pollState{Cursor: *since, SeenSIDs: seen.snapshot()}); err != nil {
			log.Printf("[sms] save state: %v", err)
		}
		if !cfg.isAllowed(msg.From) {
			log.Printf("[sms] dropping message from non-allowed sender %s", msg.From)
			continue
		}
		body := strings.TrimSpace(msg.Body)
		if body == "" {
			continue
		}
		origin := originPrefix + msg.From
		if err := m.hub.Send(ctx, body, origin); err != nil {
			// Hub busy: tell the user via SMS instead of silently
			// dropping. They can resend once the in-flight turn ends.
			// lastSender is left untouched so the in-flight turn still
			// replies to whoever started it.
			if errors.Is(err, hub.ErrBusy) {
				_, _ = client.Send(ctx, cfg.from, msg.From,
					"Diesel is in the middle of another turn — try again in a moment.")
				continue
			}
			log.Printf("[sms] hub.Send: %v", err)
			continue
		}
		// Only stash the sender after the hub accepted the turn —
		// otherwise a rejected Send could leave a stale number staged
		// for the next assistant reply.
		m.mu.Lock()
		m.lastSender = msg.From
		m.mu.Unlock()
	}
}

// dispatchLoop drains the hub subscription. When an assistant reply
// completes for an SMS-originated turn we POST it back to the
// lastSender; turn errors also generate a short SMS so the user isn't
// left wondering whether their message got through.
func (m *Manager) dispatchLoop(ctx context.Context, sub *hub.Subscriber, client *Client, cfg config) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.Events:
			if !ok {
				return
			}
			if !strings.HasPrefix(ev.Origin, originPrefix) {
				continue
			}
			switch ev.Type {
			case hub.EventTurnComplete:
				if ev.Assistant == nil {
					continue
				}
				m.replyTo(ctx, client, cfg, ev.Assistant.Content)
			case hub.EventTurnError:
				m.replyTo(ctx, client, cfg, "Sorry — something went wrong on Diesel's side: "+ev.Error)
			}
		}
	}
}

// replyTo sends `body` to the lastSender via Twilio. Clears the sender
// on success so a subsequent desktop-originated turn doesn't
// accidentally text the SMS user.
func (m *Manager) replyTo(ctx context.Context, client *Client, cfg config, body string) {
	m.mu.Lock()
	to := m.lastSender
	m.mu.Unlock()
	if to == "" || strings.TrimSpace(body) == "" {
		return
	}
	if _, err := client.Send(ctx, cfg.from, to, body); err != nil {
		log.Printf("[sms] send to %s: %v", to, err)
		m.setStatus("✗ Twilio send: " + err.Error())
		return
	}
	m.mu.Lock()
	if m.lastSender == to {
		m.lastSender = ""
	}
	m.mu.Unlock()
}

// setStatus updates the dialog-facing status string under the lock.
func (m *Manager) setStatus(s string) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

// seenSet is a bounded set of recently-seen Twilio message SIDs used
// to deduplicate poll overlap. FIFO eviction — once `cap` IDs are in
// the set, the oldest insert is removed when a new one lands. Not safe
// for concurrent use; the poll loop is the sole owner.
type seenSet struct {
	cap   int
	order []string
	set   map[string]struct{}
}

func newSeenSet(cap int) *seenSet {
	return &seenSet{cap: cap, set: make(map[string]struct{}, cap)}
}

func (s *seenSet) has(id string) bool {
	_, ok := s.set[id]
	return ok
}

func (s *seenSet) add(id string) {
	if _, ok := s.set[id]; ok {
		return
	}
	if len(s.order) >= s.cap {
		drop := s.order[0]
		s.order = s.order[1:]
		delete(s.set, drop)
	}
	s.order = append(s.order, id)
	s.set[id] = struct{}{}
}

// snapshot returns the SIDs in insertion order, oldest first. Used to
// serialize the set to disk so a restart can rebuild the same dedup
// state and FIFO eviction order.
func (s *seenSet) snapshot() []string {
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}
