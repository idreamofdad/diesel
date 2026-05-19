// Package server is the HTTP/WebSocket front end that lets a browser
// drive the same Diesel conversation the desktop GUI is using. It's a
// thin shell over a hub.Hub — every state-changing endpoint just calls
// into the hub, every read endpoint snapshots from it.
//
// Lifecycle is managed by Manager: Apply(settings) brings the listener
// up, down, or replaces it on a port/bind change. Failures keep the
// previous server running so the user is never left with no server at
// all because they typed the wrong port.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"diesel/internal/audio"
	"diesel/internal/hub"
	"diesel/internal/settings"
	"diesel/internal/util"

	"github.com/gin-gonic/gin"
)

// Manager owns the HTTP server and reacts to settings changes. The zero
// value is unusable; construct with New.
type Manager struct {
	hub      *hub.Hub
	staticFS fs.FS

	mu     sync.Mutex
	srv    *http.Server
	addr   string // last successful bind, e.g. "http://127.0.0.1:7777"
	status string // human-readable state for the Settings dialog status row
	// applied snapshots the config the current srv was started with, so
	// Apply can short-circuit when nothing material changed.
	applied serverConfig
}

// serverConfig is the subset of AppSettings the server actually cares
// about — used for change detection in Apply.
type serverConfig struct {
	enabled  bool
	expose   bool
	port     int
	token    string
}

func configFor(s settings.AppSettings) serverConfig {
	return serverConfig{
		enabled: s.EnableServer,
		expose:  s.ServerExposeNetwork,
		port:    s.ServerPort,
		token:   strings.TrimSpace(s.ServerAuthToken),
	}
}

// New returns a Manager bound to the given hub. staticFS is the
// embedded web/dist tree; pass http.Dir for dev or nil to disable static
// serving (the API routes still work).
func New(h *hub.Hub, staticFS fs.FS) *Manager {
	return &Manager{
		hub:      h,
		staticFS: staticFS,
		status:   "○ Stopped",
	}
}

// Status returns the current state for display in the Settings dialog.
func (m *Manager) Status() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Address returns the bind URL (e.g. "http://127.0.0.1:7777") of the
// running server, or "" if stopped.
func (m *Manager) Address() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addr
}

// Apply brings the server in line with the given settings. Idempotent:
// calling Apply with unchanged config is a no-op. On any failure to
// start a new listener, the previous server stays up and the returned
// status reports the bind error.
func (m *Manager) Apply(s settings.AppSettings) string {
	cfg := configFor(s)

	m.mu.Lock()
	if cfg == m.applied && m.srv != nil {
		// Same config, server already running — nothing to do.
		st := m.status
		m.mu.Unlock()
		return st
	}
	m.mu.Unlock()

	// Stop case: if the server is supposed to be off, shut it down and
	// release the port. Always exits via the same statusUpdate.
	if !cfg.enabled {
		m.stop()
		m.mu.Lock()
		m.applied = cfg
		m.status = "○ Stopped"
		st := m.status
		m.mu.Unlock()
		return st
	}

	// Build the new server first; only swap if it binds. This is what
	// lets a typo in the port field surface as an error without taking
	// the old (working) server down.
	host := "127.0.0.1"
	if cfg.expose {
		host = "0.0.0.0"
	}
	listenAddr := fmt.Sprintf("%s:%d", host, cfg.port)

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		// Bind failed: leave the old server (if any) running.
		m.mu.Lock()
		st := "✗ " + err.Error()
		m.status = st
		m.mu.Unlock()
		return st
	}

	router := m.buildRouter(cfg.token)
	newSrv := &http.Server{
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Take down the previous server before starting the new one — we've
	// already proven the bind works, so there's no risk of being stuck
	// without a listener.
	m.stop()

	go func() {
		// Serve returns http.ErrServerClosed on graceful shutdown; any
		// other error is unexpected. We don't have a logger here yet;
		// the status field is the closest thing to user-visible output.
		if err := newSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.mu.Lock()
			m.status = "✗ " + err.Error()
			m.srv = nil
			m.addr = ""
			m.mu.Unlock()
		}
	}()

	bindURL := fmt.Sprintf("http://%s:%d", host, cfg.port)
	if cfg.expose {
		// On 0.0.0.0 it's more useful to print loopback in the status
		// — the user usually wants to copy a URL they can hit.
		bindURL = fmt.Sprintf("http://127.0.0.1:%d (also LAN)", cfg.port)
	}
	m.mu.Lock()
	m.srv = newSrv
	m.addr = bindURL
	m.applied = cfg
	m.status = "● Running on " + bindURL
	st := m.status
	m.mu.Unlock()
	return st
}

// Stop shuts the server down for good. Safe to call from any goroutine
// and at any time, including when nothing is running.
func (m *Manager) Stop() {
	m.stop()
	m.mu.Lock()
	m.status = "○ Stopped"
	m.applied = serverConfig{}
	m.mu.Unlock()
}

func (m *Manager) stop() {
	m.mu.Lock()
	srv := m.srv
	m.srv = nil
	m.addr = ""
	m.mu.Unlock()
	if srv == nil {
		return
	}
	// 1 s drain — long enough for WS clients to receive the close
	// frame and any in-flight HTTP requests to finish, short enough
	// that the Settings dialog doesn't visibly hang on Save.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// buildRouter assembles the gin engine. Called fresh on every start so
// auth and middleware reflect the current settings.
func (m *Manager) buildRouter(token string) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// CORS: permissive on the API routes so the Vite dev server (running
	// on :5173) can hit the Go API during development. In production
	// (embedded UI on the same origin) this is a no-op.
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})

	api := r.Group("/api")
	api.Use(authMiddleware(token))
	api.GET("/state", m.handleState)
	api.POST("/send", m.handleSend)
	api.POST("/clear", m.handleClear)
	api.POST("/transcribe", m.handleTranscribe)
	api.GET("/portrait/:id", m.handlePortrait)
	api.GET("/portrait-preview/:id", m.handlePortraitPreview)
	api.GET("/audio/:id", m.handleAudio)
	api.GET("/ws", m.handleWS)
	api.GET("/settings", m.handleSettingsGet)
	api.POST("/settings", m.handleSettingsSave)
	api.POST("/settings/models", m.handleSettingsModels)
	api.POST("/settings/test", m.handleSettingsTest)
	api.POST("/settings/test-tts", m.handleSettingsTestTTS)

	// Static UI: serve the embedded SPA off /. The file server is wired
	// up only when the embed actually contains an index.html — fresh
	// clones with no `go generate ./...` get the built-in stub HTML
	// instead, so the user sees an actionable error instead of a blank
	// 404. Asset routes pass through to the file server; SPA deep
	// links fall back to index.html so client-side routing works.
	if m.staticFS != nil && hasIndex(m.staticFS) {
		fileServer := http.FileServer(http.FS(m.staticFS))
		r.GET("/", func(c *gin.Context) {
			c.Request.URL.Path = "/"
			fileServer.ServeHTTP(c.Writer, c.Request)
		})
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			path := strings.TrimPrefix(c.Request.URL.Path, "/")
			if path == "" {
				path = "index.html"
			}
			if f, err := m.staticFS.Open(path); err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(c.Writer, c.Request)
				return
			}
			// Asset-shaped paths (anything with a file extension) must
			// 404 cleanly rather than fall through to index.html — the
			// classic symptom is browsers parsing the SPA's HTML as
			// WebAssembly/JS/CSS and producing "expected magic word"
			// errors. SPA deep-link routes (no extension, e.g.
			// "/settings") still get the index.html fallback so
			// client-side routing keeps working.
			if looksLikeAsset(path) {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			c.Request.URL.Path = "/"
			fileServer.ServeHTTP(c.Writer, c.Request)
		})
	} else {
		r.GET("/", serveStub)
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			serveStub(c)
		})
	}

	return r
}

// hasIndex returns true when the embedded file tree contains an
// index.html — the signal that a real Vite build has been run.
func hasIndex(fsys fs.FS) bool {
	f, err := fsys.Open("index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// looksLikeAsset returns true when the path's final segment has a
// file extension. Used by the NoRoute handler to distinguish missing
// SPA deep-links (which should rewrite to index.html) from missing
// asset files (which must 404 — otherwise the browser tries to parse
// the SPA HTML as WASM/JS/CSS).
func looksLikeAsset(path string) bool {
	last := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		last = path[i+1:]
	}
	return strings.Contains(last, ".")
}

// stubHTML is what /  serves when no frontend has been built yet —
// shows the actionable command instead of a confusing 404. Kept inline
// so the server is self-contained and the message can't drift out of
// sync with what the embed expects.
const stubHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>Diesel — frontend not built</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
       background:#2b2b2b; color:#ececec; padding:3rem; line-height:1.5; }
code { background:#3a3a3a; padding:.15rem .35rem; border-radius:3px; }
.hint { color:#888; font-size:.9rem; margin-top:2rem; }
</style></head>
<body>
<h1>Diesel</h1>
<p>The web frontend hasn't been built yet.</p>
<p>From the project root, run:</p>
<p><code>go generate ./...</code></p>
<p>This invokes <code>npm ci &amp;&amp; npm run build</code> in <code>web/</code> and writes
the SPA into <code>web/dist/</code>. Restart Diesel after the build finishes.</p>
<p class="hint">Requires Node 20+ and npm. The desktop GUI works without this.</p>
</body></html>`

func serveStub(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(stubHTML))
}

// authMiddleware enforces the bearer token when one is configured. The
// browser WS API can't send custom headers on the upgrade — so we also
// accept ?token=… on the query string. Token blank = no auth.
func authMiddleware(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" {
			c.Next()
			return
		}
		got := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if got == "" {
			got = c.Query("token")
		}
		if got != token {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

// handleState returns a snapshot for a freshly-connected client so it
// can paint the transcript and status bar without waiting for the next
// event. Includes the latest portrait URL so the image panel populates
// immediately too.
func (m *Manager) handleState(c *gin.Context) {
	hist := m.hub.History()
	resp := gin.H{
		"history":   hist,
		"in_flight": m.hub.InFlight(),
		"status":    m.hub.LastStatus(),
	}
	if id, png := m.hub.LatestPortrait(); id != "" && len(png) > 0 {
		resp["portrait_url"] = "/api/portrait/" + id
	}
	c.JSON(http.StatusOK, resp)
}

// handleSend posts a user message into the hub. Returns 409 when
// another turn is in flight; the client should wait for the next
// turn_complete event before re-enabling its Send button.
func (m *Manager) handleSend(c *gin.Context) {
	var body struct {
		Text   string `json:"text"`
		Origin string `json:"origin"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty message"})
		return
	}
	if body.Origin == "" {
		body.Origin = "anonymous"
	}
	if err := m.hub.Send(c.Request.Context(), body.Text, body.Origin); err != nil {
		if errors.Is(err, hub.ErrBusy) {
			c.JSON(http.StatusConflict, gin.H{"error": "busy"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

// handleClear wipes the conversation.
func (m *Manager) handleClear(c *gin.Context) {
	if err := m.hub.Clear(c.Request.Context()); err != nil {
		if errors.Is(err, hub.ErrBusy) {
			c.JSON(http.StatusConflict, gin.H{"error": "busy"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// handleTranscribe accepts a multipart upload from the browser's
// MediaRecorder / Silero VAD output, forwards it to the configured STT
// endpoint as-is (OpenAI-compatible servers accept the codecs browsers
// produce), and on success pumps the recognized text into the hub as
// if the originating client had sent it via /send. The 'origin' form
// field carries the subscriber ID so TTS routing still works.
func (m *Manager) handleTranscribe(c *gin.Context) {
	origin := c.PostForm("origin")
	if origin == "" {
		origin = "anonymous"
	}
	header, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	f, err := header.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	s := settings.Load()
	ep := util.FirstNonEmpty(s.STTEndpoint, s.APIEndpoint)
	key := util.FirstNonEmpty(s.STTAPIKey, s.APIKey)
	m.hub.SetStatus("Transcribing…")
	text, err := audio.TranscribeBlob(c.Request.Context(), ep, key, s.STTModel, header.Filename, header.Header.Get("Content-Type"), data)
	if err != nil {
		m.hub.SetStatus("✗ " + err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		m.hub.SetStatus("No speech detected")
		c.JSON(http.StatusOK, gin.H{"text": ""})
		return
	}
	if err := m.hub.Send(c.Request.Context(), text, origin); err != nil {
		c.JSON(http.StatusOK, gin.H{"text": text, "send_error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"text": text, "sent": true})
}

// handlePortrait serves cached PNG bytes by ID. "latest" is a magic
// alias for whichever portrait is currently freshest.
func (m *Manager) handlePortrait(c *gin.Context) {
	id := c.Param("id")
	if id == "latest" {
		latest, png := m.hub.LatestPortrait()
		if latest == "" || len(png) == 0 {
			c.Status(http.StatusNotFound)
			return
		}
		c.Data(http.StatusOK, "image/png", png)
		return
	}
	png, ok := m.hub.Portrait(id)
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	c.Data(http.StatusOK, "image/png", png)
}

// handlePortraitPreview serves a cached intermediate preview frame
// (ComfyUI ships JPEG by default, sometimes PNG). Content-Type is
// sniffed because we forward whatever the upstream sent — both formats
// render fine in <img> tags and QPixmap. Frames evict quickly as new
// ones arrive, so 404 here is expected for older frames.
func (m *Manager) handlePortraitPreview(c *gin.Context) {
	id := c.Param("id")
	data, ok := m.hub.PortraitPreview(id)
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	ct := http.DetectContentType(data)
	c.Data(http.StatusOK, ct, data)
}

// handleAudio serves cached TTS audio. Content-Type is left to the
// browser to sniff because we don't know what the upstream TTS endpoint
// produced; both Chrome and Safari handle WAV/MP3/Opus without help.
func (m *Manager) handleAudio(c *gin.Context) {
	id := c.Param("id")
	data, ok := m.hub.Audio(id)
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	c.Data(http.StatusOK, "audio/mpeg", data)
}
