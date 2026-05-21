package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"diesel/internal/hub"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

// wsCommand is the shape browser clients send over the WS upstream. The
// server is mostly broadcast-driven, so commands are short: send a
// message, clear the conversation, ping the heartbeat.
type wsCommand struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// wsAckEvent is the synthetic event the server sends right after upgrade
// so clients can seed their state (history snapshot, in-flight flag,
// status string, latest portrait URL) without an additional /api/v1/state
// round-trip.
type wsAckEvent struct {
	Type        string `json:"type"`
	ClientID    string `json:"client_id"`
	Status      string `json:"status,omitempty"`
	InFlight    bool   `json:"in_flight"`
	PortraitURL string `json:"portrait_url,omitempty"`
}

// handleWS upgrades the connection and pumps events between the hub and
// the browser. Two goroutines per client: one reads commands from the
// WS into the hub; the parent pumps events from the hub out to the WS
// and handles heartbeats. Disconnect (read error, close, or context
// cancel) tears down both.
func (m *Manager) handleWS(c *gin.Context) {
	// `coder/websocket` validates Origin on upgrade by default. For an
	// app where the same Go binary serves both the API and the UI, the
	// browser origin equals the server origin — but during Vite dev,
	// the SPA lives on :5173 and pokes the Go server on :7777. We rely
	// on the bearer-token gate for auth and accept any origin so the
	// dev loop stays usable.
	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusInternalError, "") }()

	// Client identifies itself via query string so reconnects can keep
	// their identity stable (which matters for last-active TTS routing).
	clientID := strings.TrimSpace(c.Query("client_id"))
	if clientID == "" {
		clientID = fmt.Sprintf("web-%d", time.Now().UnixNano())
	}

	sub := m.hub.Subscribe(clientID)
	defer m.hub.Unsubscribe(clientID)

	// Send the initial state so the client can paint without a separate
	// /api/v1/state fetch. The web client subscribes to this ack frame and
	// requests the full history via REST when it's needed (cheap on
	// loopback, and keeps the WS frame small).
	ack := wsAckEvent{
		Type:     "ack",
		ClientID: clientID,
		Status:   m.hub.LastStatus(),
		InFlight: m.hub.InFlight(),
	}
	if id, png := m.hub.LatestPortrait(); id != "" && len(png) > 0 {
		ack.PortraitURL = "/api/v1/portrait/" + id
	}
	if raw, err := json.Marshal(ack); err == nil {
		_ = conn.Write(c.Request.Context(), websocket.MessageText, raw)
	}

	// Reader goroutine: pumps commands from the WS into the hub.
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	go m.readLoop(ctx, conn, clientID, cancel)

	// Writer loop runs in this goroutine. Heartbeat every 25 s so any
	// HTTP-aware intermediaries don't drop the connection.
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.Events:
			if !ok {
				return
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			writeCtx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, raw)
			wcancel()
			if err != nil {
				return
			}
		case <-heartbeat.C:
			pingCtx, pcancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(pingCtx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}

// readLoop blocks reading from the WS and dispatches each command into
// the hub. Any error (close, network failure) cancels the parent ctx
// so the writer loop exits cleanly. Cancellation is the only legitimate
// way to stop — the hub itself never closes a client's socket.
func (m *Manager) readLoop(ctx context.Context, conn *websocket.Conn, clientID string, cancel context.CancelFunc) {
	defer cancel()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var cmd wsCommand
		if err := json.Unmarshal(raw, &cmd); err != nil {
			continue
		}
		switch cmd.Type {
		case "send":
			text := strings.TrimSpace(cmd.Text)
			if text == "" {
				continue
			}
			if err := m.hub.Send(ctx, text, clientID); err != nil {
				if !errors.Is(err, hub.ErrBusy) {
					// Non-busy errors aren't expected from Send other
					// than empty-text guards; surface via the next
					// state poll rather than a custom error frame.
					_ = err
				}
			}
		case "clear":
			_ = m.hub.Clear(ctx)
		case "ping":
			// Application-level ping the client may send; the response
			// is implicit via the heartbeat. Nothing to do.
		default:
			// Unknown command — ignore. Wire stability is more important
			// than strict validation here; old/new client mixes shouldn't
			// disconnect.
		}
	}
}

// (Avoids an unused-import lint when the test build trims handlers.)
var _ = http.StatusOK
