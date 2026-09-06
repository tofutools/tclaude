package server

import (
	"context"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
)

type joiningWork struct{ entered, cancelled, release chan struct{} }

func (w joiningWork) ReconcilePendingWork(ctx context.Context) (app.WorkReconcileReport, error) {
	close(w.entered)
	<-ctx.Done()
	close(w.cancelled)
	<-w.release
	return app.WorkReconcileReport{}, ctx.Err()
}

func TestWorkReconciliationJoinsSettlement(t *testing.T) {
	w := joiningWork{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	stop := startWorkReconciliation(context.Background(), w)
	<-w.entered
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	<-w.cancelled
	select {
	case <-stopped:
		t.Fatal("worker returned before application settlement completed")
	default:
	}
	close(w.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("worker did not join")
	}
}
