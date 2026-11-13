package web

import (
	"context"
	"sync"

	"github.com/jgerdes5/reolink-protect-bridge/internal/bridge"
	"github.com/jgerdes5/reolink-protect-bridge/internal/config"
	"github.com/rs/zerolog"
)

// Runner owns the one running bridge and replaces it on configuration changes.
type Runner struct {
	log zerolog.Logger

	restart sync.Mutex // one restart at a time

	mu       sync.Mutex
	current  *bridge.Bridge
	cancel   context.CancelFunc
	done     chan struct{}
	startErr string
}

// NewRunner logs bridge messages to log.
func NewRunner(log zerolog.Logger) *Runner { return &Runner{log: log} }

// Restart stops the running bridge, waits until it has stopped (Protect must
// never see two cameras with one identity) and starts a new one if cfg is
// complete.
func (r *Runner) Restart(cfg config.Config) {
	r.restart.Lock()
	defer r.restart.Unlock()

	r.stop()

	if !cfg.Complete() {
		r.log.Info().Strs("missing", cfg.Missing()).Msg("[web] configuration incomplete, bridge not started")
		return
	}
	b, err := bridge.New(cfg.Options(), r.log)
	if err != nil {
		r.log.Error().Err(err).Msg("[web] bridge not started")
		r.mu.Lock()
		r.startErr = err.Error()
		r.mu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.mu.Lock()
	r.current, r.cancel, r.done, r.startErr = b, cancel, done, ""
	r.mu.Unlock()

	r.log.Info().Str("doorbell", cfg.Doorbell.Host).Str("protect", cfg.Protect.Host).Msg("[web] bridge started")
	go func() {
		defer close(done)
		b.Run(ctx)
	}()
}

// Stop stops the bridge and waits for it.
func (r *Runner) Stop() {
	r.restart.Lock()
	defer r.restart.Unlock()
	r.stop()
}

func (r *Runner) stop() {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.current, r.cancel, r.done, r.startErr = nil, nil, nil, ""
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	r.log.Info().Msg("[web] bridge stopped")
}

// Current is the running bridge (nil if none) and why it failed to start.
func (r *Runner) Current() (*bridge.Bridge, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current, r.startErr
}
