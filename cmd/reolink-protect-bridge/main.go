// Command reolink-protect-bridge turns one Reolink doorbell into a UniFi
// Protect camera, configured through its web page.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	listen := flag.String("listen", "", "web UI listen address, overrides the config for this run (e.g. :8080); normally set on the web page")
	showVersion := flag.Bool("version", false, "print the version and exit")
	healthcheck := flag.Bool("healthcheck", false, "check that the running app's web page answers (for Docker's HEALTHCHECK) and exit")
	ingress := flag.Bool("ingress", os.Getenv("RPB_INGRESS") == "1",
		"Home Assistant add-on: serve the web page only to Home Assistant's Ingress proxy, without a separate login; env RPB_INGRESS=1")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *healthcheck {
		if err := checkHealth(*dataDir, *listen); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(run(*dataDir, *listen, *ingress))
}

func run(dataDir, listen string, ingress bool) int {
	logs := logbuf.New(os.Stdout, logbuf.Size)
	level, err := zerolog.ParseLevel(strings.ToLower(envOr("RPB_LOG", "info")))
	if err != nil || level == zerolog.NoLevel {
		level = zerolog.InfoLevel
	}
	zerolog.TimeFieldFormat = time.RFC3339Nano
	log := zerolog.New(logs).Level(level).With().Timestamp().Logger()

	store, err := config.Open(dataDir)
	if err != nil {
		log.Error().Err(err).Msg("[main] load configuration")
		return 1
	}
	if v := os.Getenv("RPB_LISTEN"); v != "" && store.Created() {
		// Only seeds a new configuration (e.g. the installer's RPB_LISTEN);
		// afterwards the address is changed on the web page.
		if _, err := store.Update(func(c *config.Config) error { c.Web.Listen = v; return nil }); err != nil {
			log.Error().Err(err).Msg("[main] RPB_LISTEN")
			return 1
		}
	}
	cfg := store.Get()
	logs.SetSecrets(cfg.Secrets())

	addr := cfg.Web.Listen
	if ingress {
		// Ingress reaches a host-network add-on on the hassio network gateway;
		// listening only there keeps the page off the LAN.
		addr = ingressListen
	}
	if listen != "" {
		addr = listen
	}
	log.Info().Str("version", version).Str("config", store.Path()).Str("listen", addr).Msg("[main] reolink-protect-bridge")
	if !cfg.HasAdmin() && !ingress {
		log.Warn().Msgf("[main] first start: open the web page on %s and create the admin password", addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := web.NewRunner(store, log)
	srv := &http.Server{
		Addr: addr,
		Handler: web.New(web.Options{
			Store:   store,
			Runner:  runner,
			Logs:    logs,
			Log:     log,
			Version: version,
			Ingress: ingress,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second, // request bodies are small JSON
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	runner.Restart() // starts the bridge only if the configuration is complete

	code := 0
	select {
	case <-ctx.Done():
		log.Info().Msg("[main] shutting down")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("[main] web server")
			code = 1 // let systemd/Docker restart us
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
	return code
}

// checkHealth asks the running app's /healthz on the configured web page
// port (or listen, if given), via loopback when it listens on all addresses.
func checkHealth(dataDir, listen string) error {
	addr := listen
	if addr == "" {
		addr = config.Default().Web.Listen
		data, err := os.ReadFile(filepath.Join(dataDir, config.FileName))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var c struct {
			Web config.Web `json:"web"`
		}
		if err == nil {
			if err := json.Unmarshal(data, &c); err != nil {
				return err
			}
			if c.Web.Listen != "" {
				addr = c.Web.Listen
			}
		}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	res, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}

// ingressListen is where Home Assistant's Supervisor connects for Ingress to an
// add-on with host networking (the hassio network gateway), with the
// ingress_port from the add-on's config.yaml.
const ingressListen = "172.30.32.1:8099"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
