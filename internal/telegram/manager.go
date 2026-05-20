// Package telegram wires a Telegram bot into a hub.Hub. Inbound messages
// arrive over the Bot API's getUpdates long-poll — no public webhook, so
// it works behind NAT the same way the desktop app already does. The bot
// shares the single hub conversation with the desktop, web, and SMS
// clients; a Telegram turn interleaves into the same history.
//
// Outbound replies are addressed off each turn's hub Origin
// ("telegram:<chat_id>"), so the bridge carries no per-turn "last sender"
// state and concurrent senders never race for the reply destination.
//
// The hub processes one turn at a time. A Telegram message that lands
// while a turn is in flight is held in a bounded in-memory queue and fed
// to the hub when a slot frees; past the cap the sender gets a "busy"
// reply instead.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"diesel/internal/audio"
	"diesel/internal/hub"
	"diesel/internal/settings"
	"diesel/internal/tracing"
	"diesel/internal/util"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// subscriberID is the stable hub-subscriber ID the manager registers
// under. The dispatch loop also watches turns from other origins so it
// knows when a hub slot frees and the queue can drain.
const subscriberID = "telegram"

// originPrefix tags every hub Send origin "telegram:<chat_id>" so the
// dispatch loop can route the reply back and ignore non-Telegram turns.
const originPrefix = "telegram:"

// longPollTimeout is the getUpdates server-side wait, in seconds.
// Telegram holds the request open this long when no message is waiting.
const longPollTimeout = 50

// queueCap bounds the pending-message queue. Past this many messages
// waiting on the hub, new arrivals get a "busy" reply instead of being
// queued — keeps memory bounded if the hub wedges (LLM endpoint down).
const queueCap = 10

// typingInterval re-sends the "typing" chat action. Telegram expires the
// indicator after ~5 s, so we refresh just under that while a turn runs.
const typingInterval = 4 * time.Second

// telegramMaxMessage is the Bot API's hard limit on a sendMessage body,
// in UTF-8 code points. Longer replies (a long voice-note transcript
// echo, mainly) are split into multiple messages.
const telegramMaxMessage = 4096

// greeting is the canned reply to /start. Telegram users reflexively
// send /start first; routing the literal "/start" to the LLM produces a
// baffling answer, so the bridge intercepts it.
const greeting = "Hey — it's Diesel. Just say something and I'll answer. Voice notes work too."

// Manager owns the Telegram poll loop, the hub subscription, and the
// dispatch goroutine. Shape mirrors sms.Manager and server.Manager so
// main.go bootstraps it the same way: New(hub), then Apply(settings) at
// startup and on every settings save.
type Manager struct {
	hub *hub.Hub

	mu      sync.Mutex
	applied config
	cancel  context.CancelFunc
	status  string
}

// config is the subset of AppSettings the manager cares about — used for
// change detection in Apply so a no-op save doesn't bounce the loop.
type config struct {
	enabled bool
	token   string
	allowed []string // normalized: lower-cased, leading '@' stripped
}

// pending is one queued inbound message waiting for a free hub slot.
type pending struct {
	chatID int64
	text   string
}

// turnRef remembers where a Telegram-originated turn's reply landed so
// the portrait that turn produces later can be sent into the right chat
// as a reply to the right message. Keyed by hub turn ID.
type turnRef struct {
	chatID    int64
	textMsgID int
}

// configFor extracts the Telegram-relevant fields and normalizes the
// allow-list.
func configFor(s settings.AppSettings) config {
	allowed := make([]string, 0, len(s.TelegramAllowedUsernames))
	for _, u := range s.TelegramAllowedUsernames {
		if v := normalizeUsername(u); v != "" {
			allowed = append(allowed, v)
		}
	}
	return config{
		enabled: s.EnableTelegram,
		token:   strings.TrimSpace(s.TelegramBotToken),
		allowed: allowed,
	}
}

// equal is a structural compare used by Apply to short-circuit a no-op
// re-apply. allowed is a slice so we have to walk it.
func (c config) equal(o config) bool {
	if c.enabled != o.enabled || c.token != o.token ||
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

// validate returns an error naming the missing field so the Settings
// dialog's status row tells the user exactly what to fill in.
func (c config) validate() error {
	switch {
	case c.token == "":
		return errors.New("Bot Token is empty")
	case len(c.allowed) == 0:
		return errors.New("no allowed usernames configured")
	}
	return nil
}

// isAllowed reports whether `username` is on the allow-list. Telegram
// usernames are case-insensitive, so both sides are normalized. An empty
// username (the sender never set one) can never match — those messages
// are dropped.
func (c config) isAllowed(username string) bool {
	want := normalizeUsername(username)
	if want == "" {
		return false
	}
	for _, a := range c.allowed {
		if a == want {
			return true
		}
	}
	return false
}

// normalizeUsername strips a leading '@' and lower-cases so a username
// typed "@Alice" in the dialog matches the "alice" Telegram reports.
func normalizeUsername(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
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

// setStatus updates the dialog-facing status string under the lock.
func (m *Manager) setStatus(s string) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

// Apply brings the manager in line with the given settings. Idempotent:
// re-applying the same config is a no-op. On a config change the prior
// loop is cancelled before a new one starts so two pollers never run at
// once — a second concurrent getUpdates would draw a 409 from Telegram.
func (m *Manager) Apply(s settings.AppSettings) string {
	cfg := configFor(s)

	m.mu.Lock()
	if cfg.equal(m.applied) && m.cancel != nil {
		st := m.status
		m.mu.Unlock()
		return st
	}
	// Tear down any prior loop under the lock so a second concurrent
	// Apply can't race to start two pollers.
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
		m.hub.Unsubscribe(subscriberID)
	}
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

	// Subscription registered before the goroutine starts so the first
	// poll-triggered turn definitely sees its TurnComplete event.
	sub := m.hub.Subscribe(subscriberID)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	// "Connecting" rather than "Running" — the bot token is validated by
	// the getMe call inside run(), off the UI thread; run() flips this
	// to Running or an error once that returns.
	m.status = "● Connecting…"
	st := m.status
	m.mu.Unlock()

	go m.run(ctx, sub, cfg)
	return st
}

// Stop shuts the manager down. Safe at any time, even when already
// stopped.
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
}

// run builds the bot (NewBotAPI does a getMe, which validates the token)
// then launches the poll and dispatch loops. Bot construction is done
// here rather than in Apply so the network call stays off the Qt thread.
func (m *Manager) run(ctx context.Context, sub *hub.Subscriber, cfg config) {
	bot, err := tgbotapi.NewBotAPI(cfg.token)
	if err != nil {
		m.setStatus("✗ Telegram: " + err.Error())
		log.Printf("[telegram] bot init failed: %v", err)
		return
	}
	m.setStatus("● Running — @" + bot.Self.UserName)
	log.Printf("[telegram] connected as @%s", bot.Self.UserName)

	// incoming hands finished inbound messages from the poll loop to the
	// dispatch loop. Unbuffered: the dispatch loop classifies each one
	// (send now / queue / reject) immediately, so the queue is the only
	// place backlog accumulates.
	incoming := make(chan pending)
	go m.pollLoop(ctx, bot, cfg, incoming)
	m.dispatchLoop(ctx, sub, bot, incoming)
}

// pollLoop drives the getUpdates long-poll and feeds inbound messages
// into `incoming`. On first run (no state file) the backlog is skipped:
// we learn the latest update_id and start one past it, so enabling the
// bot doesn't replay up to 24 h of queued messages.
func (m *Manager) pollLoop(ctx context.Context, bot *tgbotapi.BotAPI, cfg config, incoming chan<- pending) {
	st, found := loadState()
	if !found {
		// offset -1 with limit 1 returns just the most recent update —
		// enough to learn where the backlog ends.
		if ups, err := bot.GetUpdates(tgbotapi.UpdateConfig{Offset: -1, Limit: 1}); err == nil && len(ups) > 0 {
			st.Offset = ups[len(ups)-1].UpdateID + 1
		}
		if err := saveState(st); err != nil {
			log.Printf("[telegram] save state: %v", err)
		}
		log.Printf("[telegram] first run — skipping backlog, starting at offset=%d", st.Offset)
	}

	u := tgbotapi.NewUpdate(st.Offset)
	u.Timeout = longPollTimeout
	updates := bot.GetUpdatesChan(u)
	log.Printf("[telegram] poll loop started: offset=%d", st.Offset)

	for {
		select {
		case <-ctx.Done():
			bot.StopReceivingUpdates()
			return
		case up, ok := <-updates:
			if !ok {
				return
			}
			m.handleUpdate(ctx, bot, cfg, up, incoming)
		}
	}
}

// handleUpdate processes one update: persist the offset, filter, and —
// for messages we accept — push a turn onto `incoming`.
//
// The offset is saved BEFORE anything else happens to the update. A
// crash after the save but before the turn completes drops the message
// rather than replaying it on restart — the same tradeoff sms/state.go
// makes, and consistent with the in-memory (non-persisted) queue.
func (m *Manager) handleUpdate(ctx context.Context, bot *tgbotapi.BotAPI, cfg config, up tgbotapi.Update, incoming chan<- pending) {
	if err := saveState(state{Offset: up.UpdateID + 1}); err != nil {
		log.Printf("[telegram] save state: %v", err)
	}

	msg := up.Message
	if msg == nil {
		// Edited messages, channel posts, callback queries — not a fresh
		// DM, so nothing to do.
		return
	}
	if msg.Chat == nil || !msg.Chat.IsPrivate() {
		// DMs only — ignore groups, supergroups, and channels.
		return
	}
	if msg.From == nil || !cfg.isAllowed(msg.From.UserName) {
		log.Printf("[telegram] dropping message from non-allowed sender @%s (chat %d)",
			usernameOf(msg.From), chatIDOf(msg))
		return
	}

	var text string
	switch {
	case msg.Voice != nil:
		t, ok := m.transcribe(ctx, bot, msg.Voice.FileID)
		if !ok {
			// STT off / download or transcription failed / empty — drop
			// silently, same as an unsupported attachment.
			return
		}
		// Echo what we heard so a misheard voice note is obvious instead
		// of producing a non-sequitur reply.
		m.send(bot, msg.Chat.ID, "🎙 “"+t+"”")
		text = t
	case strings.TrimSpace(msg.Text) != "":
		text = strings.TrimSpace(msg.Text)
		if isStartCommand(text) {
			m.send(bot, msg.Chat.ID, greeting)
			return
		}
	default:
		// Photo, sticker, document, location, … — nothing the hub can
		// take. Drop silently.
		return
	}

	log.Printf("[telegram] inbound from @%s (chat %d): %q -> queue",
		msg.From.UserName, msg.Chat.ID, text)
	select {
	case incoming <- pending{chatID: msg.Chat.ID, text: text}:
	case <-ctx.Done():
	}
}

// dispatchLoop owns the pending queue and is the sole caller of
// hub.Send for Telegram turns — no mutex needed on the queue. It reacts
// to inbound messages from the poll loop and to hub events: a turn
// completing (any origin) frees a slot, so the queue is drained then;
// a Telegram-originated turn completing sends the reply back.
func (m *Manager) dispatchLoop(ctx context.Context, sub *hub.Subscriber, bot *tgbotapi.BotAPI, incoming <-chan pending) {
	log.Printf("[telegram] dispatch loop started")
	defer log.Printf("[telegram] dispatch loop exiting")

	var queue []pending
	var typingCancel context.CancelFunc
	// awaitingPortrait maps a hub turn ID to where that turn's reply
	// landed, populated when a Telegram turn completes and consumed when
	// its portrait_ready arrives. In-memory only — turn IDs reset each
	// run, so a turn in flight across a restart is simply dropped.
	//
	// lastPortrait holds the message ID of the portrait photo currently
	// shown in each chat, so it can be deleted when the next turn
	// replaces (or clears) it. This one is persisted: without it a
	// restart would forget the posted portraits and never clean them up.
	awaitingPortrait := map[int64]turnRef{}
	lastPortrait := loadPortraitState()
	stopTyping := func() {
		if typingCancel != nil {
			typingCancel()
			typingCancel = nil
		}
	}
	defer stopTyping()

	// drain feeds queued messages to the hub one at a time. A successful
	// Send starts a turn; we stop and wait for its completion before
	// sending the next. ErrBusy means another client grabbed the slot —
	// keep the message queued and retry on the next turn-complete.
	drain := func() {
		for len(queue) > 0 {
			p := queue[0]
			origin := originPrefix + strconv.FormatInt(p.chatID, 10)
			err := m.hub.Send(ctx, p.text, origin)
			if err == nil {
				queue = queue[1:]
				return
			}
			if errors.Is(err, hub.ErrBusy) {
				return
			}
			// Anything else (e.g. empty message) won't fix itself by
			// retrying — drop it and move on.
			log.Printf("[telegram] hub.Send for chat %d: %v", p.chatID, err)
			queue = queue[1:]
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-incoming:
			if !ok {
				return
			}
			if len(queue) >= queueCap {
				log.Printf("[telegram] queue full (%d) — rejecting chat %d", len(queue), p.chatID)
				m.send(bot, p.chatID,
					"Diesel is backed up right now — try again in a moment.")
				continue
			}
			queue = append(queue, p)
			drain()
		case ev, ok := <-sub.Events:
			if !ok {
				return
			}
			chatID, mine := parseOrigin(ev.Origin)
			switch ev.Type {
			case hub.EventTurnStarted:
				if mine {
					stopTyping()
					tctx, tcancel := context.WithCancel(ctx)
					typingCancel = tcancel
					go m.typingLoop(tctx, bot, chatID)
				}
			case hub.EventTurnComplete:
				if mine {
					stopTyping()
					var textMsgID int
					if ev.Assistant != nil {
						textMsgID = m.send(bot, chatID, ev.Assistant.Content)
					}
					// Remember the turn so its portrait — which arrives
					// later on portrait_ready — can be attached to this
					// reply. Every turn emits exactly one portrait_ready
					// (empty URL when image gen is off), so this entry is
					// always consumed; it never leaks.
					awaitingPortrait[ev.TurnID] = turnRef{chatID: chatID, textMsgID: textMsgID}
				}
				drain()
			case hub.EventPortraitReady:
				// portrait_ready carries no origin, so we match it to a
				// Telegram turn by TurnID. A miss means the turn belonged
				// to the desktop/web/SMS — not ours to handle.
				ref, ok := awaitingPortrait[ev.TurnID]
				if !ok {
					break
				}
				delete(awaitingPortrait, ev.TurnID)
				if ev.PortraitURL != "" {
					newID, sent := m.sendPortrait(bot, ref, ev.PortraitURL)
					if !sent {
						// Send failed — keep the old portrait rather than
						// leave the chat with nothing.
						break
					}
					// Send-then-delete: the replacement is up before the
					// stale one comes down, so the chat never flickers
					// empty.
					m.deletePortrait(bot, lastPortrait, ref.chatID)
					lastPortrait[ref.chatID] = newID
					persistPortraits(lastPortrait)
					break
				}
				// Image gen off or the render failed — no replacement is
				// coming, so drop the now-stale portrait outright.
				m.deletePortrait(bot, lastPortrait, ref.chatID)
				persistPortraits(lastPortrait)
			case hub.EventTurnError:
				if mine {
					stopTyping()
					m.send(bot, chatID,
						"Sorry — something went wrong on Diesel's side: "+ev.Error)
				}
				drain()
			}
		}
	}
}

// typingLoop keeps the "typing…" chat action alive for chatID until ctx
// is cancelled (the turn completed). Telegram expires the indicator
// after ~5 s, so it's re-sent on a shorter interval.
func (m *Manager) typingLoop(ctx context.Context, bot *tgbotapi.BotAPI, chatID int64) {
	send := func() {
		if _, err := bot.Request(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)); err != nil {
			log.Printf("[telegram] chat action for %d: %v", chatID, err)
		}
	}
	send()
	tick := time.NewTicker(typingInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			send()
		}
	}
}

// transcribe downloads a Telegram voice note and runs it through the
// configured STT endpoint. Telegram voice notes are OGG/Opus, which
// OpenAI-compatible STT servers accept directly — no transcoding. The
// bool is false (with the failure logged) when STT is unconfigured or
// anything along the way fails; the caller drops the message silently.
func (m *Manager) transcribe(ctx context.Context, bot *tgbotapi.BotAPI, fileID string) (string, bool) {
	s := settings.Load()
	ep := util.FirstNonEmpty(s.STTEndpoint, s.APIEndpoint)
	key := util.FirstNonEmpty(s.STTAPIKey, s.APIKey)
	if strings.TrimSpace(ep) == "" {
		log.Printf("[telegram] voice note received but no STT endpoint configured")
		return "", false
	}

	url, err := bot.GetFileDirectURL(fileID)
	if err != nil {
		log.Printf("[telegram] get file URL: %v", err)
		return "", false
	}
	data, err := downloadFile(ctx, url)
	if err != nil {
		log.Printf("[telegram] download voice note: %v", err)
		return "", false
	}

	m.hub.SetStatus("Transcribing Telegram voice note…")
	text, err := audio.TranscribeBlob(ctx, ep, key, s.STTModel, "voice.ogg", "audio/ogg", data)
	if err != nil {
		log.Printf("[telegram] transcribe: %v", err)
		m.hub.SetStatus("✗ Telegram voice transcription: " + err.Error())
		return "", false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	return text, true
}

// downloadFile fetches the bytes at a Telegram file URL.
func downloadFile(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := tracing.HTTPClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, util.HTTPStatusError(resp, 256)
	}
	return io.ReadAll(resp.Body)
}

// TestConnection validates a bot token by calling getMe under a short
// timeout. Drives the Settings dialog's Test button, mirroring
// settings.TestLLMConnection / comfyui.TestConnection.
func TestConnection(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return "✗ No bot token configured."
	}
	bot, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint,
		&http.Client{Timeout: 8 * time.Second})
	if err != nil {
		return "✗ " + err.Error()
	}
	return "✓ Connected as @" + bot.Self.UserName + "."
}

// send posts text back to a Telegram chat, splitting on the Bot API's
// 4096-code-point ceiling. It returns the message ID of the first chunk
// (0 if nothing was sent) so the caller can anchor a follow-up portrait
// as a reply to it. Failures are logged and surfaced to both the dialog
// status row and the hub status bar, since the Settings dialog is
// usually closed when a send fails.
func (m *Manager) send(bot *tgbotapi.BotAPI, chatID int64, text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	var firstID int
	for _, chunk := range splitMessage(text) {
		msg, err := bot.Send(tgbotapi.NewMessage(chatID, chunk))
		if err != nil {
			log.Printf("[telegram] send to chat %d: %v", chatID, err)
			m.setStatus("✗ Telegram send: " + err.Error())
			m.hub.SetStatus(fmt.Sprintf("✗ Telegram send to chat %d failed: %s", chatID, err))
			return firstID
		}
		if firstID == 0 {
			firstID = msg.MessageID
		}
	}
	return firstID
}

// sendPortrait uploads the rendered portrait for a turn as its own photo
// message, anchored as a reply to that turn's text reply. The bytes come
// straight from the hub's cache — no HTTP round-trip. Returns the new
// photo's message ID so the caller can delete it when the next portrait
// replaces it.
func (m *Manager) sendPortrait(bot *tgbotapi.BotAPI, ref turnRef, portraitURL string) (int, bool) {
	id := strings.TrimPrefix(portraitURL, "/api/portrait/")
	data, ok := m.hub.Portrait(id)
	if !ok || len(data) == 0 {
		// The cache is small (8 entries); a slow turn could see its
		// portrait evicted before this fires. Rare — just skip it.
		log.Printf("[telegram] portrait %q not in hub cache", id)
		return 0, false
	}
	photo := tgbotapi.NewPhoto(ref.chatID, tgbotapi.FileBytes{Name: "portrait.png", Bytes: data})
	// 0 = no reply anchor, which Telegram accepts — happens only when the
	// reply itself had no text.
	photo.ReplyToMessageID = ref.textMsgID
	msg, err := bot.Send(photo)
	if err != nil {
		log.Printf("[telegram] send portrait to chat %d: %v", ref.chatID, err)
		m.setStatus("✗ Telegram portrait: " + err.Error())
		return 0, false
	}
	return msg.MessageID, true
}

// deletePortrait removes the portrait photo currently tracked for chatID,
// if any, and forgets it. Best-effort: a message older than Telegram's
// 48 h deletion window (or already gone) draws an error we just log.
func (m *Manager) deletePortrait(bot *tgbotapi.BotAPI, lastPortrait map[int64]int, chatID int64) {
	msgID, ok := lastPortrait[chatID]
	if !ok {
		return
	}
	delete(lastPortrait, chatID)
	if _, err := bot.Request(tgbotapi.NewDeleteMessage(chatID, msgID)); err != nil {
		log.Printf("[telegram] delete portrait %d in chat %d: %v", msgID, chatID, err)
	}
}

// persistPortraits writes the chat → portrait-message-ID map to disk so
// a restart can still delete a stale portrait when its replacement is
// posted. Best-effort: a failed write is logged, not fatal.
func persistPortraits(lastPortrait map[int64]int) {
	if err := savePortraitState(lastPortrait); err != nil {
		log.Printf("[telegram] save portrait state: %v", err)
	}
}

// splitMessage breaks text into Bot-API-sized chunks. Telegram counts
// UTF-8 code points, so the split is rune-aware. Diesel's replies are
// short — this only really bites on a long voice-note transcript echo.
func splitMessage(text string) []string {
	runes := []rune(text)
	if len(runes) <= telegramMaxMessage {
		return []string{text}
	}
	var out []string
	for len(runes) > 0 {
		n := telegramMaxMessage
		if n > len(runes) {
			n = len(runes)
		}
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	return out
}

// isStartCommand reports whether text is the /start command — optionally
// addressed (/start@BotName) or carrying a deep-link payload
// (/start ref123). Telegram users send this reflexively as their first
// message.
func isStartCommand(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	cmd := fields[0]
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd == "/start"
}

// parseOrigin extracts the chat ID from a hub Origin string. The bool is
// false for any origin that isn't one of ours (desktop, web, sms).
func parseOrigin(origin string) (int64, bool) {
	rest, ok := strings.CutPrefix(origin, originPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// usernameOf / chatIDOf are nil-safe accessors for log lines on the
// rejection path, where From or Chat may be absent.
func usernameOf(u *tgbotapi.User) string {
	if u == nil {
		return ""
	}
	return u.UserName
}

func chatIDOf(msg *tgbotapi.Message) int64 {
	if msg == nil || msg.Chat == nil {
		return 0
	}
	return msg.Chat.ID
}
