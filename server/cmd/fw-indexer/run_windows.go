//go:build windows

package main

import (
	"context"
	"log"

	"golang.org/x/sys/windows/svc"

	"files-workbench/v2/service"
)

// runAsServiceIfManaged runs the indexer under the Windows Service Control Manager
// when the SCM launched us (svc.IsWindowsService), translating SCM stop/shutdown
// into context cancellation. Returns false when launched interactively, so the
// normal foreground path in main() takes over.
//
// ⚠️ RUNTIME-UNVERIFIED (compile-checked only) — see service/service_windows.go.
func runAsServiceIfManaged(cfg runConfig) bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false
	}
	if err := svc.Run(service.Name, &indexerService{cfg: cfg}); err != nil {
		log.Printf("[fw-indexer] windows service run failed: %v", err)
	}
	return true
}

// indexerService adapts runIndexer to the svc.Handler protocol.
type indexerService struct{ cfg runConfig }

func (s *indexerService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- runIndexer(ctx, s.cfg) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepts}
	for {
		select {
		case req := <-r:
			switch req.Cmd {
			case svc.Interrogate:
				changes <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				<-errc
				return false, 0
			}
		case err := <-errc:
			if err != nil {
				log.Printf("[fw-indexer] indexer exited: %v", err)
				return false, 1
			}
			return false, 0
		}
	}
}
