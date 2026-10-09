//go:build windows

package main

import (
	"context"

	"golang.org/x/sys/windows/svc"
)

// runAsService runs serve under the Windows service control manager when started by it
// (docs/12 §1): it reports RUNNING once serve is up and stops serve on STOP/SHUTDOWN.
func runAsService(ctx context.Context, name string, serve func(ctx context.Context) error) error {
	inService, err := svc.IsWindowsService()
	if err != nil || !inService {
		return serve(ctx)
	}
	return svc.Run(name, &handler{ctx: ctx, serve: serve})
}

type handler struct {
	ctx   context.Context
	serve func(ctx context.Context) error
}

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan error, 1)
	go func() { done <- h.serve(ctx) }()
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			cancel()
			if err != nil {
				return true, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			default: // pause, continue, session and power events: not accepted (docs/12 §1)
			}
		}
	}
}
