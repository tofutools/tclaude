package server

import (
	"context"
	"log"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
)

// The server owns the timer and joins the worker. The application owns durable
// progress, effect admission, and whether an uncertain operation can advance.
func startWorkReconciliation(parent context.Context, work app.WorkReconciler) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if _, err := work.ReconcilePendingWork(ctx); err != nil && ctx.Err() == nil {
				log.Printf("work reconciliation failed: code=%s", app.Code(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
