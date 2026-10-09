//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zombar/tailraid/internal/config"
)

// reopenLogOnSignal reopens log.file on SIGHUP until ctx ends, so an external rotation
// (logrotate, newsyslog) sees new lines in the new file (docs/12 §1, I-124).
func reopenLogOnSignal(ctx context.Context, lf *config.LogFile, log *slog.Logger) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	defer signal.Stop(sig)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sig:
			if err := lf.Reopen(); err != nil {
				log.Error("log file reopen failed", "err", err)
				continue
			}
			log.Info("log file reopened on SIGHUP")
		}
	}
}
