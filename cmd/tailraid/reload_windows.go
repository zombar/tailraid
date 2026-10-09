//go:build windows

package main

import (
	"context"
	"log/slog"

	"github.com/zombar/tailraid/internal/config"
)

// reopenLogOnSignal is a no-op on Windows: there is no SIGHUP, and the log file rotates
// itself (docs/12 §1).
func reopenLogOnSignal(context.Context, *config.LogFile, *slog.Logger) {}
