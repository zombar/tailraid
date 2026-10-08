//go:build !windows

package main

import "context"

// runAsService is a plain run outside Windows: launchd and systemd supervise processes
// without an in-process protocol.
func runAsService(ctx context.Context, _ string, serve func(ctx context.Context) error) error {
	return serve(ctx)
}
