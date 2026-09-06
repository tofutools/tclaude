package server

import (
	"context"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
)

type heldRenewal struct {
	app.ExecutionAccessLifecycle
	started, cancelled, release chan struct{}
}

func (r *heldRenewal) SweepExecutionAccess(ctx context.Context) app.ExecutionAccessRenewalReport {
	close(r.started)
	<-ctx.Done()
	close(r.cancelled)
	<-r.release // Simulate host settlement after cancellation was observed.
	return app.ExecutionAccessRenewalReport{}
}

func TestRenewalShutdownCancelsAndJoinsActiveSweep(t *testing.T) {
	r := &heldRenewal{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	stop := startAccessRenewal(context.Background(), r)
	defer stop()
	defer close(r.release)
	select {
	case <-r.started:
	case <-time.After(time.Second):
		t.Fatal("renewal worker did not start")
	}
	joined := make(chan struct{})
	go func() { stop(); close(joined) }()
	select {
	case <-r.cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel active renewal")
	}
	select {
	case <-joined:
		t.Fatal("shutdown returned before renewal settlement")
	default:
	}
}
