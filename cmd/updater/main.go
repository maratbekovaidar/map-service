// Command updater builds new versions of the map data (extract, OSRM, tiles,
// Nominatim, Photon), verifies them and switches the stack over.
//
//	updater run  [-only s1,s2] [-version V] [-keep-on-failure]   one update, then exit
//	updater cron                                          update on UPDATE_CRON
//
// Exit codes: 0 success, 1 failure (the live version is untouched),
// 2 usage/configuration error, 3 another update is already running.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"map-service/internal/config"
	"map-service/internal/pipeline"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitBusy    = 3
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	log := newLogger()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: updater run|cron [flags]")
		return exitUsage
	}
	cmd, rest := args[0], args[1:]

	cfg, err := pipeline.LoadConfig(config.OS)
	if err != nil {
		log.Error("configuration error", "err", err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		only := fs.String("only", "", "comma-separated steps to run (debugging; never switches the live version)")
		resume := fs.String("version", "", "resume this existing build directory (debugging, use with -only)")
		keep := fs.Bool("keep-on-failure", false, "keep the build directory and candidate containers after a failure")
		if err := fs.Parse(rest); err != nil {
			return exitUsage
		}
		opts := pipeline.Options{KeepOnFailure: *keep, Version: *resume}
		if *only != "" {
			opts.Only = strings.Split(*only, ",")
		}
		version, err := pipeline.New(cfg, log).Update(ctx, opts)
		switch {
		case errors.Is(err, pipeline.ErrAlreadyRunning):
			log.Error("update not started", "err", err)
			return exitBusy
		case err != nil:
			log.Error("update failed; the live version is unchanged", "err", err)
			return exitFailure
		}
		log.Info("update succeeded", "version", version)
		return exitOK
	case "cron":
		log.Info("scheduler started", "cron", cfg.Cron, "city", cfg.Name)
		if err := pipeline.New(cfg, log).RunCron(ctx); err != nil {
			log.Error("scheduler stopped", "err", err)
			return exitFailure
		}
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want run or cron)\n", cmd)
		return exitUsage
	}
}

func newLogger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "text") {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
