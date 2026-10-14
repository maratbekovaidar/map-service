// Command gateway is the API gateway of the map stack: auth, rate limiting,
// CORS, caching and normalisation in front of Photon, Nominatim and OSRM, plus
// vector tile and map style delivery.
//
//	gateway               run the server (configuration: environment, see .env.example)
//	gateway healthcheck   probe /healthz of the local instance (exit 0/1), for Docker
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"map-service/internal/cache"
	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
	"map-service/internal/clients/photon"
	"map-service/internal/config"
	"map-service/internal/dataset"
	"map-service/internal/httpapi"
	"map-service/internal/tiles"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadGateway(config.OS)
	if err != nil {
		// Logged by the caller; slog is not configured yet (it needs the level).
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Live data version: drives the cache key prefix and /readyz.
	watcher := dataset.NewWatcher(cfg.DataRoot, cfg.DataVersionTTL)
	go watcher.Run(ctx)

	var store cache.Store = cache.Noop{}
	if cfg.RedisAddr != "" {
		r := cache.NewRedis(cache.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB}, log)
		defer r.Close()
		store = r
	} else {
		log.Warn("REDIS_ADDR is empty: running without a cache")
	}

	tileServer, err := tiles.NewServer(cfg.DataRoot, 32, log)
	if err != nil {
		return err
	}

	routers := make(map[string]httpapi.RouteAPI, len(cfg.OSRMURLs))
	for profile, base := range cfg.OSRMURLs {
		routers[profile] = osrm.New(base, cfg.RouteTimeout)
	}
	api := httpapi.New(httpapi.Deps{
		Cfg:       cfg,
		Log:       log,
		Photon:    photon.New(cfg.PhotonURL, cfg.UpstreamTimeout),
		Nominatim: nominatim.New(cfg.NominatimURL, cfg.UpstreamTimeout),
		OSRM:      routers,
		Cache:     store,
		Tiles:     tileServer,
		Version:   watcher.Version,
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	// Request contexts are deliberately not derived from the signal context:
	// on SIGTERM in-flight requests may finish within ShutdownTimeout.

	errc := make(chan error, 1)
	go func() {
		log.Info("gateway listening", "addr", cfg.ListenAddr, "city", cfg.Name, "bbox", cfg.BBox.String(),
			"profiles", cfg.Profiles, "data_version", watcher.Version(), "cache", store.Enabled())
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	log.Info("shutting down", "grace", cfg.ShutdownTimeout.String())
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Error("graceful shutdown incomplete, closing connections", "err", err)
		_ = srv.Close()
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// healthcheck calls /healthz of the instance on the configured port.
func healthcheck() int {
	addr := os.Getenv("GATEWAY_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.Status)
		return 1
	}
	return 0
}
