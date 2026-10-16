// Command trips is the trip tracking service: a driver's app creates a trip
// and reports GPS positions, the service follows the car along its route and
// rebuilds the route when the car leaves it, and a watcher with the trip's
// watch token sees where the car is.
//
//	trips               run the server (configuration: environment, see .env.example)
//	trips healthcheck   probe /healthz of the local instance (exit 0/1), for Docker
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"map-service/internal/clients/osrm"
	"map-service/internal/config"
	"map-service/internal/dataset"
	"map-service/internal/httpkit"
	"map-service/internal/trips/api"
	"map-service/internal/trips/store"
	"map-service/internal/trips/tracker"
)

const (
	shutdownTimeout = 20 * time.Second
	// Map matching runs while a trip is locked, so it gets less time than a
	// route: a slow answer only means the tracker decides by distance.
	matchTimeout  = 3 * time.Second
	sweepInterval = time.Minute
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "trips:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadTrips(config.OS)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The database may still be starting (compose waits for it to be
	// healthy, other deployments may not): retry for a while.
	var db *store.Store
	for attempt := 1; ; attempt++ {
		db, err = store.Open(ctx, cfg.DatabaseURL)
		if err == nil || attempt == 30 || ctx.Err() != nil {
			break
		}
		log.Warn("database not reachable yet", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return err
	}
	defer db.Close()
	applied, err := db.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		log.Info("database migrated", "applied", applied)
	}

	watcher := dataset.NewWatcher(cfg.DataRoot, cfg.DataVersionTTL)
	go watcher.Run(ctx)

	router := osrm.New(cfg.OSRMURL, cfg.RouteTimeout)
	matcher := tracker.OSRMMatcher{OSRM: osrm.New(cfg.OSRMURL, matchTimeout), Profile: config.TripsProfile}
	srv := api.New(api.Deps{
		Cfg: cfg, Log: log, Store: db, Router: router,
		Tracker: tracker.New(tracker.DefaultConfig(), matcher),
		Version: watcher.Version,
	})
	go srv.RunSweeper(ctx, sweepInterval)
	go srv.RunListener(ctx) // wakes the event streams

	hs := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	// Event streams never finish on their own; end them so that shutdown
	// only waits for ordinary requests. EventSource reconnects elsewhere.
	hs.RegisterOnShutdown(srv.CloseStreams)
	errc := make(chan error, 1)
	go func() {
		log.Info("trips listening", "addr", cfg.ListenAddr, "city", cfg.Name, "osrm", cfg.OSRMURL,
			"data_version", watcher.Version(), "idle_timeout", cfg.IdleTimeout.String(), "retention", cfg.Retention.String())
		errc <- hs.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	log.Info("shutting down", "grace", shutdownTimeout.String())
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		log.Error("graceful shutdown incomplete, closing connections", "err", err)
		_ = hs.Close()
		return err
	}
	log.Info("shutdown complete")
	return nil
}

func healthcheck() int {
	addr := os.Getenv("TRIPS_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if err := httpkit.ProbeHealthz(addr); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	return 0
}
