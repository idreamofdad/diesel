package knowledge

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"diesel/internal/logging"
	"diesel/internal/settings"
	"diesel/internal/storage"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var logger = logging.Component("knowledge")

// Service owns the knowledge graph's MCP plumbing for the lifetime of the app:
// one in-process server backed by SQLite, an always-on in-memory client
// session the chat loop drives the LLM's tools through, and an opt-in
// streamable-HTTP listener (loopback, token-gated, off by default) that lets
// external MCP clients edit the same graph. The HTTP side follows the same
// Apply/Stop manager shape as the SMS/Telegram/Matrix bridges so main.go can
// hot-reapply it on every settings Save.
type Service struct {
	store  *Store
	server *mcp.Server
	bridge *Bridge

	// In-memory session lifetime — closed on Stop.
	clientSession *mcp.ClientSession
	serverSession *mcp.ServerSession

	mu      sync.Mutex
	applied httpConfig
	httpSrv *http.Server
	status  string
}

// httpConfig is the slice of settings the HTTP listener depends on; Apply
// recreates the listener only when one of these changes.
type httpConfig struct {
	enabled bool
	port    int
	token   string
}

func httpConfigFrom(s settings.AppSettings) httpConfig {
	return httpConfig{
		enabled: s.KnowledgeMCPEnableHTTP,
		port:    s.KnowledgeMCPPort,
		token:   strings.TrimSpace(s.KnowledgeMCPAuthToken),
	}
}

// New builds the service: it wires the SQLite-backed graph store, the MCP
// server, and the in-memory client session used for the model's tool calls.
// The HTTP listener is not started here — call Apply for that. s seeds the
// example names baked into the server's instructions and tool descriptions
// (a snapshot — a later name change needs a restart to surface).
func New(store *storage.Store, s settings.AppSettings) (*Service, error) {
	kn := NewStore(store.SQLDB())
	server := NewServer(kn, s)

	// Connect server and client over a paired in-memory transport. Both
	// sessions run for the whole app; Stop closes them.
	serverT, clientT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		return nil, fmt.Errorf("knowledge: connect server session: %w", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "diesel-chat", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		_ = serverSession.Close()
		return nil, fmt.Errorf("knowledge: connect client session: %w", err)
	}
	logger.Debug().Msg("service initialized: in-memory MCP server+client sessions connected")

	return &Service{
		store:         kn,
		server:        server,
		bridge:        &Bridge{store: kn, session: clientSession},
		clientSession: clientSession,
		serverSession: serverSession,
		status:        "Ready",
	}, nil
}

// Bridge returns the chat.KnowledgeBase the hub passes into chat.Completion.
func (s *Service) Bridge() *Bridge { return s.bridge }

// Store exposes the graph store for the UI editor (direct CRUD, bypassing the
// model). Mutations made here are seen by the next system-prompt injection.
func (s *Service) Store() *Store { return s.store }

// Status returns the current HTTP-listener status string for the settings UI.
func (s *Service) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Apply (re)configures the external HTTP listener from settings and returns
// the resulting status string (mirroring the other managers so the settings
// dialog can show it). It is idempotent: an unchanged config is a no-op, a
// changed one tears down the old listener and starts a new one, and disabling
// stops it. The in-memory session the model uses is independent of this and
// always available.
func (s *Service) Apply(set settings.AppSettings) string {
	cfg := httpConfigFrom(set)
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg == s.applied && (s.httpSrv != nil) == cfg.enabled {
		return s.status
	}
	s.stopHTTPLocked()
	s.applied = cfg
	if !cfg.enabled {
		s.status = "Disabled"
		return s.status
	}
	if cfg.port <= 0 {
		s.status = "✗ invalid port"
		return s.status
	}
	// Token gating is mandatory for the network door — without it any local
	// process could rewrite the model's memory. The internal in-memory session
	// is unaffected, so this only blocks the optional listener.
	if cfg.token == "" {
		s.status = "✗ set an auth token to enable the HTTP listener"
		logger.Warn().Msg("HTTP listener not started: auth token is empty")
		return s.status
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.server }, nil,
	)
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           bearerAuth(cfg.token, handler),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.status = "✗ " + err.Error()
		logger.Error().Err(err).Msgf("listen %s", addr)
		return s.status
	}
	s.httpSrv = srv
	s.status = fmt.Sprintf("✓ Listening on %s", addr)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error().Err(err).Msg("http serve")
		}
	}()
	return s.status
}

// Stop shuts down the HTTP listener (if any) and closes the in-memory MCP
// sessions. Safe to call multiple times.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopHTTPLocked()
	s.applied = httpConfig{}
	s.status = "Disabled"
	s.mu.Unlock()

	if s.clientSession != nil {
		_ = s.clientSession.Close()
	}
	if s.serverSession != nil {
		_ = s.serverSession.Close()
	}
}

// stopHTTPLocked shuts down the listener with a short grace period. Caller
// holds s.mu.
func (s *Service) stopHTTPLocked() {
	if s.httpSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(ctx)
	s.httpSrv = nil
}

// bearerAuth wraps h so every request must carry "Authorization: Bearer
// <token>". Mirrors the static-token posture of the main HTTP server.
func bearerAuth(token string, h http.Handler) http.Handler {
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
