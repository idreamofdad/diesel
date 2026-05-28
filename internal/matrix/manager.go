// Package matrix wires a Matrix account into a hub.Hub. Inbound events
// arrive over the homeserver's /sync long-poll — same NAT-friendly
// posture as the Telegram bridge. The bot operates in E2EE rooms;
// Olm/Megolm sessions live in diesel.db alongside the rest of the
// bridge state, managed by mautrix-go's crypto helper.
//
// Outbound replies are addressed off each turn's hub Origin
// ("matrix:<room_id>"), so the bridge carries no per-turn "last sender"
// state and concurrent rooms never race for the reply destination.
//
// The hub processes one turn at a time; messages that land while a
// turn is in flight wait in a bounded in-memory queue and feed the hub
// when a slot frees. Past the cap the sender gets a "busy" reply.
//
// Authorization is two-layered: the bot only responds to messages from
// the single configured MatrixAllowedUser, AND only in rooms whose
// joined-membership is exactly the two of them. A third member silences
// the bot in that room until the membership drops back to two.
package matrix

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"diesel/internal/audio"
	"diesel/internal/hub"
	"diesel/internal/settings"
	"diesel/internal/storage"
	"diesel/internal/util"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// subscriberID is the stable hub-subscriber ID the manager registers
// under. The dispatch loop also watches turns from other origins so it
// knows when a hub slot frees and the queue can drain.
const subscriberID = "matrix"

// originPrefix tags every hub Send origin "matrix:<room_id>" so the
// dispatch loop can route the reply back and ignore non-Matrix turns.
const originPrefix = "matrix:"

// queueCap bounds the pending-message queue. Past this many messages
// waiting on the hub, new arrivals get a "busy" reply instead of being
// queued — keeps memory bounded if the hub wedges (LLM endpoint down).
const queueCap = 10

// typingInterval re-sends m.typing. Matrix expires the presence after
// the timeout we pass; refreshing slightly under the timeout keeps the
// indicator alive across a long turn.
const (
	typingInterval = 4 * time.Second
	typingTimeout  = 5 * time.Second
)

// initialDeviceName is the human-readable label the homeserver attaches
// to our login session. Shown in Element's "Sessions" list so the user
// can recognise (and revoke) the Diesel device.
const initialDeviceName = "Diesel"

// greeting is the canned reply posted on first join to a room — Matrix
// has no /start convention so this is the analogue of Telegram's
// /start-intercept message.
const greeting = "Hey — it's Diesel. Just say something and I'll answer. Voice notes work too."

// Manager owns the Matrix sync loop, the hub subscription, and the
// dispatch goroutine. Shape mirrors telegram.Manager and sms.Manager
// so main.go bootstraps it the same way: New(hub, store), then
// Apply(settings) at startup and on every settings save.
type Manager struct {
	hub   *hub.Hub
	store *storage.Store

	mu      sync.Mutex
	applied config
	cancel  context.CancelFunc
	status  string
}

// config is the subset of AppSettings the manager cares about — used
// for change detection in Apply so a no-op save doesn't bounce the loop.
// botMXID and allowed are pre-normalized to "@localpart:server".
type config struct {
	enabled  bool
	botMXID  string
	password string
	allowed  string
}

// pending is one queued inbound message waiting for a free hub slot.
type pending struct {
	roomID  id.RoomID
	eventID id.EventID
	text    string
}

// turnRef remembers which room a Matrix-originated turn belonged to,
// so the portrait that turn produces later can be sent into the same
// room. Keyed by hub turn ID.
type turnRef struct {
	roomID id.RoomID
}

// configFor extracts the Matrix-relevant fields and normalizes the
// user IDs into canonical "@localpart:server" form.
func configFor(s settings.AppSettings) config {
	return config{
		enabled:  s.EnableMatrix,
		botMXID:  normalizeMXID(s.MatrixBotUserID),
		password: s.MatrixPassword,
		allowed:  normalizeMXID(s.MatrixAllowedUser),
	}
}

// equal is a structural compare used by Apply to skip a goroutine bounce
// on a no-op save.
func (c config) equal(o config) bool {
	return c.enabled == o.enabled && c.botMXID == o.botMXID &&
		c.password == o.password && c.allowed == o.allowed
}

// validate returns an error naming the missing/invalid field so the
// Settings dialog's status row tells the user exactly what to fix.
func (c config) validate() error {
	switch {
	case c.botMXID == "":
		return errors.New("bot user ID is empty")
	case !isValidMXID(c.botMXID):
		return errors.New("bot user ID must look like @name:server")
	case c.password == "":
		return errors.New("password is empty")
	case c.allowed == "":
		return errors.New("no allowed user configured")
	case !isValidMXID(c.allowed):
		return errors.New("allowed user must look like @name:server")
	case strings.EqualFold(c.botMXID, c.allowed):
		return errors.New("bot user and allowed user must differ")
	}
	return nil
}

// isAllowed reports whether `sender` matches the configured allowed
// MXID. Matrix localparts are case-insensitive in practice; server
// names are technically case-sensitive but we treat them
// case-insensitively too, matching what Element does in its UI.
func (c config) isAllowed(sender id.UserID) bool {
	return c.allowed != "" && strings.EqualFold(string(sender), c.allowed)
}

// normalizeMXID trims whitespace and prepends a leading '@' when the
// user typed "alice:server" without the sigil. The localpart is
// lower-cased; the server name is left as-typed (homeservers handle
// case-insensitivity themselves) but compared case-insensitively.
func normalizeMXID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "@") {
		s = "@" + s
	}
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return s
	}
	local := strings.ToLower(s[:colon])
	return local + s[colon:]
}

// isValidMXID checks the bare structural shape "@localpart:server".
// Stricter validation happens server-side at login.
func isValidMXID(s string) bool {
	if len(s) < 4 || s[0] != '@' {
		return false
	}
	colon := strings.IndexByte(s, ':')
	return colon > 1 && colon < len(s)-1
}

// serverPart returns everything after the ':' in a normalized MXID.
// Empty on a malformed input — callers should validate first.
func serverPart(mxid string) string {
	i := strings.IndexByte(mxid, ':')
	if i < 0 || i == len(mxid)-1 {
		return ""
	}
	return mxid[i+1:]
}

// New returns a stopped Manager bound to the given hub and store.
// Apply must be called to start it.
func New(h *hub.Hub, store *storage.Store) *Manager {
	return &Manager{
		hub:    h,
		store:  store,
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
// loop is cancelled before a new one starts so two pollers never run
// concurrently — a second /sync from the same device would race for
// next_batch advancement.
func (m *Manager) Apply(s settings.AppSettings) string {
	cfg := configFor(s)

	m.mu.Lock()
	if cfg.equal(m.applied) && m.cancel != nil {
		st := m.status
		m.mu.Unlock()
		return st
	}
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
	// sync-triggered turn definitely sees its TurnComplete event.
	sub := m.hub.Subscribe(subscriberID)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	// "Connecting" — the homeserver round-trip happens off the UI
	// thread; run() flips this to Running or an error once login
	// succeeds.
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

// run is the supervisor: it builds the mautrix client, performs login
// (via the crypto helper, which also restores any existing device's
// Olm sessions), then launches the sync and dispatch loops. All
// homeserver round-trips happen here, off the UI thread.
func (m *Manager) run(ctx context.Context, sub *hub.Subscriber, cfg config) {
	homeserverURL, err := m.resolveHomeserver(ctx, cfg.botMXID)
	if err != nil {
		m.setStatus("✗ Matrix: " + err.Error())
		log.Printf("[matrix] homeserver discovery failed: %v", err)
		return
	}

	client, err := mautrix.NewClient(homeserverURL, "", "")
	if err != nil {
		m.setStatus("✗ Matrix: " + err.Error())
		log.Printf("[matrix] client init failed: %v", err)
		return
	}
	// mautrix-go logs verbosely via zerolog by default. Silence it; the
	// bridge surfaces the same info via the stdlib log prefix used by
	// the rest of Diesel.
	client.Log = zerolog.Nop()

	// Pickle key persists once and is reused on every restart — a fresh
	// key would render the existing Olm sessions undecryptable.
	pickleKey, ok := loadPickleKey(ctx, m.store)
	if !ok {
		pickleKey, err = generatePickleKey()
		if err != nil {
			m.setStatus("✗ Matrix: " + err.Error())
			return
		}
		if err := savePickleKey(ctx, m.store, pickleKey); err != nil {
			log.Printf("[matrix] save pickle key: %v", err)
		}
	}

	// Share diesel.db with mautrix's crypto + state stores. The driver
	// name only needs to round-trip through ParseDialect — modernc.org/
	// sqlite's "sqlite" prefix resolves to the SQLite dialect, and
	// dbutil uses the pre-opened pool directly.
	cryptoDB, err := dbutil.NewWithDB(m.store.SQLDB(), "sqlite")
	if err != nil {
		m.setStatus("✗ Matrix: " + err.Error())
		log.Printf("[matrix] dbutil: %v", err)
		return
	}

	// Bridge inbound messages from sync callbacks to the dispatch
	// loop. Buffered so a small burst doesn't stall /sync; past the
	// buffer the gateAndEnqueue select drops with a log line.
	incoming := make(chan pending, queueCap)
	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	m.registerSyncHandlers(syncer, client, cfg, incoming)

	helper, err := cryptohelper.NewCryptoHelper(client, pickleKey, cryptoDB)
	if err != nil {
		m.setStatus("✗ Matrix: " + err.Error())
		log.Printf("[matrix] crypto helper: %v", err)
		return
	}
	helper.LoginAs = &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: cfg.botMXID},
		Password:                 cfg.password,
		InitialDeviceDisplayName: initialDeviceName,
	}
	if err := helper.Init(ctx); err != nil {
		m.setStatus("✗ Matrix login: " + err.Error())
		log.Printf("[matrix] login failed: %v", err)
		return
	}
	client.Crypto = helper
	defer func() {
		if err := helper.Close(); err != nil {
			log.Printf("[matrix] crypto helper close: %v", err)
		}
	}()
	if err := saveHomeserverURL(ctx, m.store, homeserverURL); err != nil {
		log.Printf("[matrix] save homeserver: %v", err)
	}

	m.setStatus("● Running — " + string(client.UserID))
	log.Printf("[matrix] logged in as %s device=%s", client.UserID, client.DeviceID)

	// Skip backlog on first ever run: if the sync store has no
	// next_batch yet, do a single sync with timeout=0 to learn the
	// current head and persist it. Subsequent restarts find a populated
	// token and resume normally — picking up messages that arrived
	// while the app was down.
	if err := m.skipBacklogIfFirstRun(ctx, client); err != nil {
		log.Printf("[matrix] skip backlog: %v", err)
	}

	go m.syncLoop(ctx, client)
	m.dispatchLoop(ctx, sub, client, incoming)
}

// resolveHomeserver figures out the homeserver base URL for the bot's
// MXID. Tries the in-DB cache first to avoid the .well-known fetch on
// every restart; falls back to discovery, then to "https://<server>"
// when discovery returns 404 (homeservers that don't serve .well-known).
func (m *Manager) resolveHomeserver(ctx context.Context, botMXID string) (string, error) {
	server := serverPart(botMXID)
	if server == "" {
		return "", errors.New("bot user ID has no server part")
	}
	if cached := loadHomeserverURL(ctx, m.store); cached != "" {
		return cached, nil
	}
	wk, err := mautrix.DiscoverClientAPI(ctx, server)
	if err != nil {
		return "", err
	}
	if wk != nil && wk.Homeserver.BaseURL != "" {
		return wk.Homeserver.BaseURL, nil
	}
	// .well-known returned 404; fall back to the bare server domain.
	return "https://" + server, nil
}

// skipBacklogIfFirstRun is the Matrix analogue of the Telegram bridge's
// "offset -1 with limit 1" trick. Without it the bot's first /sync
// after a fresh install would replay every unread message in every
// room it's a member of, all at once.
func (m *Manager) skipBacklogIfFirstRun(ctx context.Context, client *mautrix.Client) error {
	since, err := client.Store.LoadNextBatch(ctx, client.UserID)
	if err != nil {
		return err
	}
	if since != "" {
		return nil
	}
	resp, err := client.SyncRequest(ctx, 0, "", "", false, event.PresenceOnline)
	if err != nil {
		return err
	}
	if resp == nil || resp.NextBatch == "" {
		return nil
	}
	log.Printf("[matrix] first run — skipping backlog, starting at %s", resp.NextBatch)
	return client.Store.SaveNextBatch(ctx, client.UserID, resp.NextBatch)
}

// registerSyncHandlers attaches our event-handling logic to the
// client's syncer. The cryptohelper hooks itself onto the same syncer
// during its Init (called after this), so encrypted events arrive
// here already decrypted (re-dispatched as EventMessage), and any
// to-device key-sharing traffic is handled transparently.
func (m *Manager) registerSyncHandlers(syncer *mautrix.DefaultSyncer, client *mautrix.Client, cfg config, incoming chan<- pending) {
	// Invites: only the configured allowed user can invite the bot.
	// Anyone else is left dangling (we never reject, just ignore) so a
	// stray invite doesn't trigger a noisy round-trip.
	syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
		if evt.GetStateKey() != string(client.UserID) {
			return
		}
		if evt.Content.AsMember().Membership != event.MembershipInvite {
			return
		}
		if !cfg.isAllowed(evt.Sender) {
			log.Printf("[matrix] ignoring invite to %s from non-allowed sender %s",
				evt.RoomID, evt.Sender)
			return
		}
		if _, err := client.JoinRoomByID(ctx, evt.RoomID); err != nil {
			log.Printf("[matrix] join %s: %v", evt.RoomID, err)
			return
		}
		log.Printf("[matrix] joined %s after invite from %s", evt.RoomID, evt.Sender)
		if _, err := client.SendText(ctx, evt.RoomID, greeting); err != nil {
			log.Printf("[matrix] greeting %s: %v", evt.RoomID, err)
		}
	})

	// Messages: text and audio. Encrypted events show up here as their
	// decrypted plaintext type via the cryptohelper's reinject hook,
	// so a single handler covers both encrypted and unencrypted rooms.
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		m.handleMessage(ctx, client, cfg, incoming, evt)
	})
}

// syncLoop runs client.SyncWithContext until the parent context is
// cancelled. mautrix retries transient failures internally; an
// MUnknownToken pulls us out, which means the access token was
// revoked — the user has to fix it in Settings.
func (m *Manager) syncLoop(ctx context.Context, client *mautrix.Client) {
	log.Printf("[matrix] sync loop started")
	defer log.Printf("[matrix] sync loop exiting")
	err := client.SyncWithContext(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		m.setStatus("✗ Matrix sync: " + err.Error())
		log.Printf("[matrix] sync error: %v", err)
	}
}

// handleMessage is the sync callback for m.room.message events. It
// filters self-echo and non-allowed senders, then routes accepted
// messages through the 2-member gate to the dispatch loop. Sync
// callbacks are synchronous on the sync goroutine, so anything slow
// (voice-note transcribe) is fanned out to its own goroutine to keep
// /sync responsive.
func (m *Manager) handleMessage(ctx context.Context, client *mautrix.Client, cfg config, incoming chan<- pending, evt *event.Event) {
	if evt.Sender == client.UserID {
		return
	}
	if !cfg.isAllowed(evt.Sender) {
		log.Printf("[matrix] dropping message from non-allowed sender %s in %s",
			evt.Sender, evt.RoomID)
		return
	}
	content := evt.Content.AsMessage()
	if content == nil {
		return
	}
	switch content.MsgType {
	case event.MsgText:
		text := strings.TrimSpace(content.Body)
		if text == "" {
			return
		}
		m.enqueueGated(ctx, client, incoming, evt.RoomID, evt.ID, text)
	case event.MsgAudio:
		// Transcribe off the sync goroutine so /sync stays responsive.
		go m.transcribeAndEnqueue(client, incoming, evt, content)
	default:
		// Image, video, file, sticker, location, … — nothing the hub
		// can take. Drop silently.
	}
}

// downloadAttachment fetches the bytes for content.URL (unencrypted)
// or content.File.URL (encrypted, decrypted with the per-file key).
// Matrix media is small enough that we hold the whole blob in memory.
func (m *Manager) downloadAttachment(ctx context.Context, client *mautrix.Client, content *event.MessageEventContent) ([]byte, error) {
	if content.File != nil {
		mxc, err := content.File.URL.Parse()
		if err != nil {
			return nil, err
		}
		ciphertext, err := client.DownloadBytes(ctx, mxc)
		if err != nil {
			return nil, err
		}
		ef := &content.File.EncryptedFile
		return ef.Decrypt(ciphertext)
	}
	if content.URL == "" {
		return nil, errors.New("attachment has no URL")
	}
	mxc, err := content.URL.Parse()
	if err != nil {
		return nil, err
	}
	return client.DownloadBytes(ctx, mxc)
}

// enqueueGated is the last filter before the hub. It enforces the
// strict 2-member room rule by polling the homeserver's joined-members
// view — cheap, and avoids maintaining our own cache that could drift
// out of sync with the server.
func (m *Manager) enqueueGated(ctx context.Context, client *mautrix.Client, incoming chan<- pending, roomID id.RoomID, eventID id.EventID, text string) {
	resp, err := client.JoinedMembers(ctx, roomID)
	if err != nil {
		log.Printf("[matrix] joined members for %s: %v", roomID, err)
		return
	}
	if len(resp.Joined) != 2 {
		log.Printf("[matrix] gating message in %s — joined=%d (only 2-member rooms allowed)",
			roomID, len(resp.Joined))
		return
	}
	select {
	case incoming <- pending{roomID: roomID, eventID: eventID, text: text}:
	case <-ctx.Done():
	default:
		log.Printf("[matrix] incoming buffer full — dropping message in %s", roomID)
	}
}

// transcribeAndEnqueue downloads an m.audio attachment, feeds the
// bytes to STT, echoes the heard text back, and enqueues the
// transcript as a normal turn. Errors drop the message silently.
func (m *Manager) transcribeAndEnqueue(client *mautrix.Client, incoming chan<- pending, evt *event.Event, content *event.MessageEventContent) {
	ctx := context.Background()
	data, err := m.downloadAttachment(ctx, client, content)
	if err != nil {
		log.Printf("[matrix] download voice in %s: %v", evt.RoomID, err)
		return
	}
	s := settings.Load()
	ep := util.FirstNonEmpty(s.STTEndpoint, s.APIEndpoint)
	key := util.FirstNonEmpty(s.STTAPIKey, s.APIKey)
	if strings.TrimSpace(ep) == "" {
		log.Printf("[matrix] voice received but no STT endpoint configured")
		return
	}
	mime := "audio/ogg"
	if content.Info != nil && content.Info.MimeType != "" {
		mime = content.Info.MimeType
	}
	m.hub.SetStatus("Transcribing Matrix voice note…")
	text, err := audio.TranscribeBlob(ctx, ep, key, s.STTModel, "voice", mime, data)
	if err != nil {
		log.Printf("[matrix] transcribe: %v", err)
		m.hub.SetStatus("✗ Matrix voice transcription: " + err.Error())
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if _, err := client.SendText(ctx, evt.RoomID, "🎙 “"+text+"”"); err != nil {
		log.Printf("[matrix] echo voice in %s: %v", evt.RoomID, err)
	}
	m.enqueueGated(ctx, client, incoming, evt.RoomID, evt.ID, text)
}

// dispatchLoop owns the pending queue and is the sole caller of
// hub.Send for Matrix turns — no mutex needed on the queue. It reacts
// to inbound messages from the sync callbacks and to hub events: a
// turn completing (any origin) frees a slot, so the queue is drained
// then; a Matrix-originated turn completing sends the reply back.
func (m *Manager) dispatchLoop(ctx context.Context, sub *hub.Subscriber, client *mautrix.Client, incoming <-chan pending) {
	log.Printf("[matrix] dispatch loop started")
	defer log.Printf("[matrix] dispatch loop exiting")

	var queue []pending
	// inFlight is whichever pending we most recently handed to the
	// hub. The hub processes one turn at a time, so we only ever have
	// one outstanding; this lets EventTurnComplete know which inbound
	// message ID to use as the m.in_reply_to anchor on the reply.
	var inFlight *pending
	var typingCancel context.CancelFunc
	awaitingPortrait := map[int64]turnRef{}
	lastPortrait := loadPortraitState(ctx, m.store)

	stopTyping := func() {
		if typingCancel != nil {
			typingCancel()
			typingCancel = nil
		}
	}
	defer stopTyping()

	drain := func() {
		for len(queue) > 0 {
			p := queue[0]
			origin := originPrefix + string(p.roomID)
			err := m.hub.Send(ctx, p.text, origin, true)
			if err == nil {
				inFlight = &p
				queue = queue[1:]
				return
			}
			if errors.Is(err, hub.ErrBusy) {
				return
			}
			log.Printf("[matrix] hub.Send for %s: %v", p.roomID, err)
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
				log.Printf("[matrix] queue full (%d) — rejecting %s", len(queue), p.roomID)
				if _, err := client.SendText(ctx, p.roomID,
					"Diesel is backed up right now — try again in a moment."); err != nil {
					log.Printf("[matrix] busy reply to %s: %v", p.roomID, err)
				}
				continue
			}
			queue = append(queue, p)
			drain()
		case ev, ok := <-sub.Events:
			if !ok {
				return
			}
			roomID, mine := parseOrigin(ev.Origin)
			switch ev.Type {
			case hub.EventTurnStarted:
				if mine {
					stopTyping()
					tctx, tcancel := context.WithCancel(ctx)
					typingCancel = tcancel
					go m.typingLoop(tctx, client, roomID)
				}
			case hub.EventTurnComplete:
				if mine {
					stopTyping()
					// Capture the user's incoming event ID before
					// clearing inFlight — used only for the read
					// receipt, not to anchor the reply (which is a
					// plain standalone message, not a quote).
					var readUpTo id.EventID
					if inFlight != nil && inFlight.roomID == roomID {
						readUpTo = inFlight.eventID
					}
					inFlight = nil
					if ev.Assistant != nil {
						replyID := m.sendReply(ctx, client, roomID, ev.Assistant.Content)
						if readUpTo != "" && replyID != "" {
							// Mark the user's message as read once the
							// reply lands so Diesel shows the same
							// "seen" affordance Element gives a human.
							if err := client.MarkRead(ctx, roomID, readUpTo); err != nil {
								log.Printf("[matrix] mark read in %s: %v", roomID, err)
							}
						}
					}
					awaitingPortrait[ev.TurnID] = turnRef{roomID: roomID}
				}
				drain()
			case hub.EventPortraitReady:
				ref, ok := awaitingPortrait[ev.TurnID]
				if !ok {
					break
				}
				delete(awaitingPortrait, ev.TurnID)
				if ev.PortraitURL != "" {
					newID, sent := m.sendPortrait(ctx, client, ref, ev.PortraitURL)
					if !sent {
						break
					}
					m.redactPortrait(ctx, client, lastPortrait, ref.roomID)
					lastPortrait[ref.roomID] = newID
					m.persistPortraits(ctx, lastPortrait)
					break
				}
				m.redactPortrait(ctx, client, lastPortrait, ref.roomID)
				m.persistPortraits(ctx, lastPortrait)
			case hub.EventTurnError:
				if mine {
					stopTyping()
					inFlight = nil
					if _, err := client.SendText(ctx, roomID,
						"Sorry — something went wrong on Diesel's side: "+ev.Error); err != nil {
						log.Printf("[matrix] error reply to %s: %v", roomID, err)
					}
				}
				drain()
			}
		}
	}
}

// typingLoop keeps the m.typing presence alive for roomID until ctx is
// cancelled (the turn completed). Matrix expires the indicator after
// the timeout we pass; refreshing slightly under that keeps it
// continuously on for the duration of a slow LLM call.
func (m *Manager) typingLoop(ctx context.Context, client *mautrix.Client, roomID id.RoomID) {
	send := func(on bool) {
		t := time.Duration(0)
		if on {
			t = typingTimeout
		}
		if _, err := client.UserTyping(ctx, roomID, on, t); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[matrix] typing in %s: %v", roomID, err)
		}
	}
	send(true)
	defer send(false)
	tick := time.NewTicker(typingInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			send(true)
		}
	}
}

// sendReply posts the assistant's reply into roomID as a plain
// standalone message — no m.in_reply_to relation, so Element et al.
// render it as a normal post instead of a quoted reply. Encryption is
// automatic when the room is encrypted: mautrix's SendMessageEvent
// checks StateStore.IsEncrypted and encrypts via the configured
// CryptoHelper.
func (m *Manager) sendReply(ctx context.Context, client *mautrix.Client, roomID id.RoomID, text string) id.EventID {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	content := event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
	}
	resp, err := client.SendMessageEvent(ctx, roomID, event.EventMessage, content)
	if err != nil {
		log.Printf("[matrix] send reply to %s: %v", roomID, err)
		m.setStatus("✗ Matrix send: " + err.Error())
		m.hub.SetStatus("✗ Matrix send to " + string(roomID) + " failed: " + err.Error())
		return ""
	}
	return resp.EventID
}

// sendPortrait uploads the rendered portrait for a turn and posts it
// as a standalone m.image in the room — no m.in_reply_to relation, so
// it sits in the timeline as a regular image post rather than a
// quoted reply. In an encrypted room the file bytes are wrapped with
// a per-file key (m.file media encryption) and the surrounding event
// is encrypted with the room's Megolm session by mautrix.
func (m *Manager) sendPortrait(ctx context.Context, client *mautrix.Client, ref turnRef, portraitURL string) (id.EventID, bool) {
	portraitID := strings.TrimPrefix(portraitURL, "/api/v1/portrait/")
	data, ok := m.hub.Portrait(portraitID)
	if !ok || len(data) == 0 {
		log.Printf("[matrix] portrait %q not in hub cache", portraitID)
		return "", false
	}

	encrypted := false
	if client.StateStore != nil {
		isEnc, err := client.StateStore.IsEncrypted(ctx, ref.roomID)
		if err == nil {
			encrypted = isEnc
		}
	}

	content := event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    "portrait.png",
		Info:    &event.FileInfo{MimeType: "image/png", Size: len(data)},
	}

	if encrypted {
		ef := attachment.NewEncryptedFile()
		ciphertext := ef.Encrypt(data)
		upload, err := client.UploadBytes(ctx, ciphertext, "application/octet-stream")
		if err != nil {
			log.Printf("[matrix] upload portrait to %s: %v", ref.roomID, err)
			m.setStatus("✗ Matrix portrait: " + err.Error())
			return "", false
		}
		content.File = &event.EncryptedFileInfo{
			EncryptedFile: *ef,
			URL:           upload.ContentURI.CUString(),
		}
	} else {
		upload, err := client.UploadBytes(ctx, data, "image/png")
		if err != nil {
			log.Printf("[matrix] upload portrait to %s: %v", ref.roomID, err)
			m.setStatus("✗ Matrix portrait: " + err.Error())
			return "", false
		}
		content.URL = upload.ContentURI.CUString()
	}

	resp, err := client.SendMessageEvent(ctx, ref.roomID, event.EventMessage, content)
	if err != nil {
		log.Printf("[matrix] send portrait to %s: %v", ref.roomID, err)
		m.setStatus("✗ Matrix portrait: " + err.Error())
		return "", false
	}
	return resp.EventID, true
}

// redactPortrait removes the previous portrait from a room, if any.
// Matrix's analogue of Telegram's "deleteMessage": a server-side
// tombstone. Best-effort — a redact failure (perms, gone) is logged.
func (m *Manager) redactPortrait(ctx context.Context, client *mautrix.Client, lastPortrait map[id.RoomID]id.EventID, roomID id.RoomID) {
	prev, ok := lastPortrait[roomID]
	if !ok {
		return
	}
	delete(lastPortrait, roomID)
	if _, err := client.RedactEvent(ctx, roomID, prev); err != nil {
		log.Printf("[matrix] redact portrait %s in %s: %v", prev, roomID, err)
	}
}

// persistPortraits writes the room → portrait-event-ID map. Best-effort:
// a failed write means a restart might leak one stale portrait when its
// replacement lands.
func (m *Manager) persistPortraits(ctx context.Context, lastPortrait map[id.RoomID]id.EventID) {
	if err := savePortraitState(ctx, m.store, lastPortrait); err != nil {
		log.Printf("[matrix] save portraits: %v", err)
	}
}

// parseOrigin extracts the room ID from a hub Origin string. The bool
// is false for any origin that isn't one of ours (desktop, web, sms,
// telegram). Room IDs start with '!' and contain a colon — we don't
// validate that here, only the prefix and non-empty remainder.
func parseOrigin(origin string) (id.RoomID, bool) {
	rest, ok := strings.CutPrefix(origin, originPrefix)
	if !ok || rest == "" {
		return "", false
	}
	return id.RoomID(rest), true
}

// TestConnection validates the bot credentials by performing a login,
// confirming the resolved user ID via /whoami, then logging out so the
// probe doesn't leave a stray device behind. Drives the Settings
// dialog's Test button — same shape as telegram.TestConnection.
func TestConnection(botMXID, password string) string {
	botMXID = normalizeMXID(botMXID)
	if botMXID == "" {
		return "✗ No bot user ID configured."
	}
	if !isValidMXID(botMXID) {
		return "✗ Bot user ID must look like @name:server."
	}
	if strings.TrimSpace(password) == "" {
		return "✗ No password configured."
	}

	server := serverPart(botMXID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wk, err := mautrix.DiscoverClientAPI(ctx, server)
	if err != nil {
		return "✗ " + err.Error()
	}
	homeserverURL := "https://" + server
	if wk != nil && wk.Homeserver.BaseURL != "" {
		homeserverURL = wk.Homeserver.BaseURL
	}
	client, err := mautrix.NewClient(homeserverURL, "", "")
	if err != nil {
		return "✗ " + err.Error()
	}
	client.Log = zerolog.Nop()
	client.Client = &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Login(ctx, &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: botMXID},
		Password:                 password,
		InitialDeviceDisplayName: initialDeviceName + " (test)",
		StoreCredentials:         true,
	})
	if err != nil {
		return "✗ " + err.Error()
	}
	whoami, err := client.Whoami(ctx)
	if err != nil {
		_, _ = client.Logout(ctx)
		return "✗ whoami: " + err.Error()
	}
	if _, err := client.Logout(ctx); err != nil {
		log.Printf("[matrix] test connection logout: %v", err)
	}
	return "✓ Connected as " + string(whoami.UserID) + " (device " + string(resp.DeviceID) + ")."
}
