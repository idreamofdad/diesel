// Package app holds the wiring shared by both entrypoints — the Fyne
// desktop app (cmd/diesel) and the headless daemon (cmd/dieseld). It opens
// the database, injects settings persistence, starts the hub and tracing,
// and stands up the bridge managers, so the two mains can't drift on setup.
//
// It is deliberately cgo-free (no Fyne, no audio device code), so the
// daemon can import it under CGO_ENABLED=0.
package app

import (
	"context"
	"fmt"
	"log"
	"time"

	"diesel/internal/hub"
	"diesel/internal/matrix"
	"diesel/internal/server"
	"diesel/internal/settings"
	"diesel/internal/sms"
	"diesel/internal/storage"
	"diesel/internal/telegram"
	"diesel/internal/tracing"
	"diesel/internal/util"
	dieselweb "diesel/web"
)

// Deps are the wired, long-lived components both entrypoints build their UI
// or serving loop around. The bridge managers arrive already Applied from
// persisted settings; Server arrives un-applied so each entrypoint decides
// how it listens — the desktop honors the EnableServer setting, the daemon
// forces it on.
type Deps struct {
	Hub      *hub.Hub
	Server   *server.Manager
	SMS      *sms.Manager
	Telegram *telegram.Manager
	Matrix   *matrix.Manager
}

// Wire opens diesel.db (at the path util.ConfigFilePath resolves — honor any
// -data-dir override before calling), injects it as the settings backend,
// starts tracing and the hub, and creates the bridge managers (SMS/Telegram/
// Matrix Applied from settings; Server left un-applied). The returned cleanup
// tears everything down in dependency order and must be called once on exit.
func Wire(ctx context.Context) (*Deps, func(), error) {
	dbPath, err := util.ConfigFilePath("diesel.db")
	if err != nil {
		return nil, nil, fmt.Errorf("config path: %w", err)
	}
	store, err := storage.Open(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open storage: %w", err)
	}

	// settings can't import storage (storage imports settings), so wire
	// persistence in by injection.
	settings.SetBackend(
		func() settings.AppSettings {
			s, err := store.LoadSettings(ctx)
			if err != nil {
				log.Printf("[settings] load: %v", err)
			}
			return s
		},
		func(s settings.AppSettings) error {
			return store.SaveSettings(ctx, s)
		},
	)

	// OpenTelemetry: a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT (or the
	// trace-specific override) is set.
	traceShutdown, err := tracing.Init(ctx)
	if err != nil {
		log.Printf("[tracing] init failed: %v", err)
	}

	h := hub.New(store)
	h.Start(ctx)

	srv := server.New(h, dieselweb.DistFS())
	smsMgr := sms.New(h, store)
	smsMgr.Apply(settings.Load())
	tgMgr := telegram.New(h, store)
	tgMgr.Apply(settings.Load())
	mxMgr := matrix.New(h, store)
	mxMgr.Apply(settings.Load())

	deps := &Deps{Hub: h, Server: srv, SMS: smsMgr, Telegram: tgMgr, Matrix: mxMgr}

	cleanup := func() {
		// Stop the server and bridges first so no in-flight handler touches a
		// half-torn-down hub, then stop the hub and close the database.
		srv.Stop()
		smsMgr.Stop()
		tgMgr.Stop()
		mxMgr.Stop()
		h.Stop()
		_ = store.Close()
		if traceShutdown != nil {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := traceShutdown(sctx); err != nil {
				log.Printf("[tracing] shutdown: %v", err)
			}
		}
	}
	return deps, cleanup, nil
}
