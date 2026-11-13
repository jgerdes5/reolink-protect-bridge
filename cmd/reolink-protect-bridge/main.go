// Command reolink-protect-bridge turns one Reolink doorbell into a UniFi
// Protect camera, configured through its web page.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/internal/config"
	"github.com/jgerdes5/reolink-protect-bridge/internal/logbuf"
	"github.com/jgerdes5/reolink-protect-bridge/internal/web"
	"github.com/rs/zerolog"
)

var version = "dev" // -ldflags "-X main.version=..."

func main() {
	dataDir := flag.String("data", envOr("RPB_DATA", "./data"), "data directory (config.json); env RPB_DATA")
	listen := flag.String("listen", "", "web UI listen address, overrides the config (e.g. :8080)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	logs := logbuf.New(os.Stdout, logbuf.Size)
	level, err := zerolog.ParseLevel(strings.ToLower(envOr("RPB_LOG", "info")))
	if err != nil || level == zerolog.NoLevel {
		level = zerolog.InfoLevel
	}
	zerolog.TimeFieldFormat = time.RFC3339Nano
	log := zerolog.New(logs).Level(level).With().Timestamp().Logger()

	store, err := config.Open(*dataDir)
	if err != nil {
		log.Fatal().Err(err).Msg("[main] load configuration")
	}
	cfg := store.Get()
	logs.SetSecrets(cfg.Secrets())

	addr := cfg.Web.Listen
	if *listen != "" {
		addr = *listen
	}
	log.Info().Str("version", version).Str("config", store.Path()).Str("listen", addr).Msg("[main] reolink-protect-bridge")
	if !cfg.HasAdmin() {
		log.Warn().Msgf("[main] first start: open the web page on %s and create the admin password", addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := web.NewRunner(log)
	srv := &http.Server{
		Addr: addr,
		Handler: web.New(web.Options{
			Store:   store,
			Runner:  runner,
			Logs:    logs,
			Log:     log,
			Version: version,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	runner.Restart(cfg) // starts the bridge only if the configuration is complete

	select {
	case <-ctx.Done():
		log.Info().Msg("[main] shutting down")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("[main] web server")
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	done := make(chan struct{})
	go func() {
		runner.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Warn().Msg("[main] bridge did not stop in time")
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
