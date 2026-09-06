package server

import (
	"context"
	"log"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
)

// Composition owns scheduling and lifetime. The application chooses which
// credentials are due and proves resource ownership before each renewal.
func startAccessRenewal(parent context.Context, access app.ExecutionAccessLifecycle) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			work, cancelWork := context.WithTimeout(ctx, 30*time.Second)
			report := access.SweepExecutionAccess(work)
			cancelWork()
			for _, failure := range report.Failed {
				// Raw provider diagnostics may contain private resource details.
				log.Printf("execution access renewal failed: execution=%s code=%s", failure.ExecutionID, failure.Code)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
