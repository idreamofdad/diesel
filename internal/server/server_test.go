package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"diesel/internal/hub"
	"diesel/internal/settings"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { gin.SetMode(gin.TestMode) }

// TestAuthMiddleware_NoTokenAllows verifies the blank-token "no auth"
// path. With ServerAuthToken="" every request should pass.
func TestAuthMiddleware_NoTokenAllows(t *testing.T) {
	h := hub.New()
	m := New(h, nil)
	r := m.buildRouter("")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestAuthMiddleware_TokenEnforced — when configured, both the
// Authorization header and the ?token= query form must work, and
// missing/wrong tokens get 401.
func TestAuthMiddleware_TokenEnforced(t *testing.T) {
	h := hub.New()
	m := New(h, nil)
	r := m.buildRouter("secret")

	cases := []struct {
		name string
		mod  func(*http.Request)
		want int
	}{
		{"no token", func(*http.Request) {}, http.StatusUnauthorized},
		{"wrong header", func(req *http.Request) { req.Header.Set("Authorization", "Bearer wrong") }, http.StatusUnauthorized},
		{"right header", func(req *http.Request) { req.Header.Set("Authorization", "Bearer secret") }, http.StatusOK},
		{"right query", func(req *http.Request) { req.URL.RawQuery = "token=secret" }, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
			tc.mod(req)
			r.ServeHTTP(w, req)
			assert.Equal(t, tc.want, w.Code)
		})
	}
}

// TestHandleSend_Busy — second Send while one is in flight returns 409.
// We don't actually run the LLM; instead we put the hub into the in-flight
// state by holding the lock through a manual mutation.
func TestHandleSend_Busy(t *testing.T) {
	h := hub.New()
	m := New(h, nil)
	r := m.buildRouter("")

	// Push the hub into "in flight" by sending a turn; it'll fail
	// quickly because no LLM is configured, but the second call
	// before the goroutine cleans up should observe busy. Race-y in
	// theory; in practice the hub holds inFlight across the whole
	// http call to chat.Completion (which will fail synchronously
	// here because the endpoint is empty).
	//
	// Simpler: drive the busy state directly.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	// Send once; expect 202 (accepted) or some flavor of accepted.
	body := `{"text":"hello","origin":"test"}`
	w1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w1, req1)
	// The hub's Send synchronously sets inFlight before returning.
	require.Equal(t, http.StatusAccepted, w1.Code)

	// Second send before the goroutine completes — busy.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusConflict, w2.Code)
}

// TestHandleState_ReturnsHistory verifies the snapshot endpoint
// reflects whatever the hub currently holds.
func TestHandleState_ReturnsHistory(t *testing.T) {
	h := hub.New()
	m := New(h, nil)
	r := m.buildRouter("")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["in_flight"])
	assert.Equal(t, "Ready", resp["status"])
}

// TestManager_Apply_StartStop verifies the lifecycle: enabled brings
// the listener up on the configured port; disabled tears it down.
func TestManager_Apply_StartStop(t *testing.T) {
	// Pick a free port so we don't collide with anything.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	h := hub.New()
	m := New(h, nil)

	s := settings.AppSettings{EnableServer: true, ServerPort: port}
	status := m.Apply(s)
	assert.Contains(t, status, "Running")

	// Hit the server to confirm it's actually listening.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+itoa(port)+"/api/state", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Apply disabled — server should stop.
	status = m.Apply(settings.AppSettings{EnableServer: false, ServerPort: port})
	assert.Contains(t, status, "Stopped")

	// Now confirm the port is free.
	ln2, err := net.Listen("tcp", "127.0.0.1:"+itoa(port))
	require.NoError(t, err, "port should be released after Apply(disabled)")
	ln2.Close()
}

// TestManager_Apply_BindFailureKeepsPriorRunning — a failed bind on
// Apply leaves the previous server in place so the user can fix the
// port without losing connectivity.
func TestManager_Apply_BindFailureKeepsPriorRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	goodPort := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	// Grab another port and hold it so the second Apply collides.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()
	badPort := blocker.Addr().(*net.TCPAddr).Port

	h := hub.New()
	m := New(h, nil)
	defer m.Stop()

	status := m.Apply(settings.AppSettings{EnableServer: true, ServerPort: goodPort})
	require.Contains(t, status, "Running")

	// Apply with a port we know is taken — should report error and
	// keep the prior listener up.
	status = m.Apply(settings.AppSettings{EnableServer: true, ServerPort: badPort})
	assert.True(t, strings.HasPrefix(status, "✗"), "expected error status, got %q", status)
	assert.Contains(t, m.Address(), itoa(goodPort), "old server should still be reachable")
}

// itoa avoids importing strconv just for the two-call sites above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
