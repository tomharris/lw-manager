// Package logging configures the process-wide slog handler.
//
// Invariant: all output goes through slog. No fmt.Println in library code —
// agent logs are the only forensic record when a device wedges overnight.
package logging

import (
	"log/slog"
	"os"

	"github.com/tomharris/lw-manager/internal/config"
)

// Setup builds a handler from cfg and installs it as the default logger.
// It returns the logger so callers can attach per-component attributes.
func Setup(cfg config.LogConfig) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Level}

	var h slog.Handler
	if cfg.Format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}

	l := slog.New(h)
	slog.SetDefault(l)
	return l
}
