// Command dieseld is the headless Diesel daemon: the hub, the HTTP server
// (which serves the web UI and the /api/v1 surface), and the SMS/Telegram/
// Matrix bridges — no native window, no native audio. The browser drives
// the full voice loop (VAD, STT, TTS playback), so nothing is lost versus
// the desktop app except the local window.
//
// It contains no cgo: build it static with
//
//	CGO_ENABLED=0 go build -tags goolm ./cmd/dieseld
//
// Bridge credentials are host-bound and configured out of band (they live in
// the same diesel.db the desktop app writes); the daemon applies whatever is
// already configured there at startup.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	dieselapp "diesel/internal/app"
	"diesel/internal/settings"
	"diesel/internal/util"
)

// defaultDaemonPort is used when neither -port nor a saved ServerPort gives
// a usable value.
const defaultDaemonPort = 8080

func main() {
	dataDir := flag.String("data-dir", "", "directory for Diesel's data (database); defaults to the OS user config dir")
	port := flag.Int("port", 0, "HTTP port to listen on (0 = use the saved setting, else 8080)")
	listenAll := flag.Bool("listen-all", false, "bind 0.0.0.0 (reachable on the network) instead of loopback only")
	authToken := flag.String("auth-token", "", "bearer token required for API access (overrides the saved setting; empty keeps it)")
	flag.Parse()
	if *dataDir != "" {
		util.SetConfigDir(*dataDir)
	}

	// Stop on Ctrl-C or SIGTERM (systemd/docker stop) so shutdown is graceful.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, cleanup, err := dieselapp.Wire(context.Background())
	if err != nil {
		log.Fatalf("[startup] %v", err)
	}

	// The daemon's whole purpose is to serve, so force the HTTP server on and
	// resolve its bind from flags, falling back to the saved settings. We
	// persist the resolved config because a settings save from the web UI
	// preserves the server fields verbatim from disk — persisting EnableServer
	// here keeps a later web Save from re-applying EnableServer=false and
	// stopping the server out from under the operator.
	s := settings.Load()
	s.EnableServer = true
	switch {
	case *port > 0:
		s.ServerPort = *port
	case s.ServerPort <= 0:
		s.ServerPort = defaultDaemonPort
	}
	s.ServerExposeNetwork = *listenAll
	if *authToken != "" {
		s.ServerAuthToken = *authToken
	}
	if err := s.Save(); err != nil {
		log.Printf("[settings] persist server config: %v", err)
	}
	if status := deps.Server.Apply(s); strings.HasPrefix(status, "✗") {
		cleanup()
		log.Fatalf("[server] %s", status)
	}

	log.Printf("Diesel daemon listening on %s", deps.Server.Address())
	if s.ServerAuthToken == "" && *listenAll {
		log.Printf("[server] WARNING: exposed on the network with no auth token — set -auth-token")
	}

	<-ctx.Done()
	log.Println("shutting down…")
	cleanup()
}
