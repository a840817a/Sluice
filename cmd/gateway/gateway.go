package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/httpapi"
	"github.com/a840817a/sluice/internal/store"
	"github.com/a840817a/sluice/internal/web"
)

// gateway is the fully composed gateway: store, manager, HTTP server and the
// router that serves them, with every cross-component callback already wired.
//
// It exists so that composition is testable. main() adds only what a test
// cannot meaningfully assert on — flag parsing, logging, TLS selection,
// ListenAndServe and shutdown — so anything that can silently go missing
// (see newGateway's OnVODReady note) lives here instead.
type gateway struct {
	cfg     config.Config
	store   *channel.Store
	manager *channel.Manager
	server  *httpapi.Server
	// handler is the router; main wraps it in an *http.Server.
	handler http.Handler
}

// newGateway builds the gateway from cfg: it opens the channel store, wires the
// manager to it, registers the VOD snapshot callback, starts every enabled
// stored channel, and builds the router with the embedded web UI handlers.
//
// ctx governs the channels started here and the admin server's restarts; it
// outlives newGateway. Cancelling it stops ingest, so callers that keep serving
// must keep it alive for the process lifetime.
//
// Order matters: OnVODReady is registered before any channel starts. A
// transition that completes during the restore loop would otherwise write
// nothing, and the omission stays silent until a restart serves an empty
// channel.
func newGateway(ctx context.Context, cfg config.Config) (*gateway, error) {
	// --- Data directory ---
	//
	// Before anything opens it. A data directory the process cannot write is a
	// deployment mistake that nothing downstream reports in time: the channel
	// store only reads at startup, /healthz never touches the directory, and a
	// container therefore reaches "healthy" and fails on the first segment
	// write instead — with an error that names a path deep inside the store
	// rather than the mount that caused it. Refusing here trades a running but
	// useless gateway for a startup failure that says what is wrong.
	if err := store.CheckWritable(cfg.Store.DataDir); err != nil {
		return nil, err
	}

	// --- Channel store (channels.json) ---
	storePath := filepath.Join(cfg.Store.DataDir, "channels.json")
	chStore, err := channel.NewStore(storePath)
	if err != nil {
		return nil, fmt.Errorf("channel store init: %w", err)
	}

	// --- Channel manager ---
	manager := channel.NewManager(cfg)
	manager.SetStore(chStore) // enables VOD state persistence across restarts
	srv := httpapi.NewServer(cfg, manager)

	// Snapshot the final MPD to disk after every VOD transition, so a restarted
	// gateway can serve the channel without re-fetching upstream. This must be
	// registered before any channel starts, or a transition that completes
	// during startup writes nothing and the omission is silent until a restart.
	manager.SetOnVODReady(srv.SaveVODManifest)

	// Start all previously persisted channels.
	for id, chCfg := range chStore.List() {
		if !chCfg.Enabled {
			continue
		}
		if _, err := manager.Start(ctx, id, chCfg); err != nil {
			slog.Error("channel start failed", "id", id, "err", err)
		}
	}

	// --- HTTP routing ---
	adminSrv := httpapi.NewAdminServerFromContext(ctx, manager, chStore)
	handler := httpapi.NewRouter(cfg, srv, adminSrv, httpapi.UI{
		Admin:        web.AdminHandler(),
		Player:       web.PlayerHandler(),
		PlayerAssets: web.PlayerAssetsHandler(),
	})

	return &gateway{
		cfg:     cfg,
		store:   chStore,
		manager: manager,
		server:  srv,
		handler: handler,
	}, nil
}
