package server

import (
	"context"
	"log"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
)

func startMessageNotifications(parent context.Context, notifications app.MessageNotificationReconciler) func() {
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
			if err := notifications.ReconcileMessageNotifications(ctx); err != nil && ctx.Err() == nil {
				log.Printf("message notification reconciliation failed: code=%s", app.Code(err))
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
