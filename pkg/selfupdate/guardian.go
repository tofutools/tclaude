package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

// SupervisedChild is the external process boundary. Healthy must establish
// readiness of this particular child and verify the hub ID and expected version.
type SupervisedChild interface {
	Stop(context.Context) error
	Healthy(context.Context, string) error
}
type Guardian struct {
	mu       sync.Mutex
	service  *Service
	child    SupervisedChild
	launch   func(context.Context, string) (SupervisedChild, error)
	binary   Binary
	deadline time.Duration
	context  context.Context
	fatal    chan error
	outcome  func(Job)
}

func NewGuardian(ctx context.Context, dir string, binary Binary, launch func(context.Context, string) (SupervisedChild, error), outcome func(Job), audits ...func(Job) error) (*Guardian, error) {
	g := &Guardian{fatal: make(chan error, 1), context: ctx, binary: binary, launch: launch, deadline: 60 * time.Second, outcome: outcome}
	var beforeStart func(Job) error
	if len(audits) > 1 {
		beforeStart = audits[1]
	}
	var started func(Job) error
	var progress func(Job)
	if len(audits) > 0 {
		started = audits[0]
		progress = func(j Job) { _ = audits[0](j) }
	}
	svc, err := New(dir, []Binary{binary}, Hooks{BeforeStart: beforeStart, Started: started, Progress: progress, Supervised: true, ReleaseOnly: true, NoDowngrade: true, Restart: g.restart, Finished: func(j Job) {
		if j.State != "restarting" && outcome != nil {
			outcome(j)
		}
	}})
	if err != nil {
		return nil, err
	}
	svc.status.CurrentVersion = binary.Version
	svc.status.InstallMethod = binary.Method
	svc.updateAvailable()
	g.service = svc
	return g, nil
}
func (g *Guardian) Service() *Service { return g.service }
func (g *Guardian) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.child != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = g.child.Stop(ctx)
	}
}
func (g *Guardian) Start() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	job := g.service.Pending()
	if job != nil && (job.State == "running" || job.State == "restarting") {
		switch job.Phase {
		case "replacing", "restarting", "health_check", "rolling_back", "restoring_backup":
			return g.recover(*job)
		default:
			if err := g.service.FinishSupervised(g.binary.Version, false, fmt.Errorf("hub guardian restarted before binary replacement")); err != nil {
				return err
			}
		}
	}
	child, err := g.launch(g.context, g.binary.Path)
	if err != nil {
		return err
	}
	g.child = child
	ctx, cancel := context.WithTimeout(g.context, g.deadline)
	defer cancel()
	return child.Healthy(ctx, g.binary.Version)
}
func (g *Guardian) restart() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	job := g.service.Pending()
	if job == nil {
		return fmt.Errorf("missing supervised restart job")
	}
	err := g.recover(*job)
	if err != nil {
		if g.child != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = g.child.Stop(ctx)
			cancel()
		}
		select {
		case g.fatal <- err:
		default:
		}
	}
	return err
}
func (g *Guardian) recover(job Job) error {
	deadline := time.Now().UTC().Add(g.deadline)
	if job.Deadline != nil && job.Deadline.Before(deadline) {
		deadline = *job.Deadline
	}
	if err := g.service.HealthDeadline(deadline); err != nil {
		return err
	}
	if g.child != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := g.child.Stop(ctx)
		cancel()
		if err != nil {
			return err
		}
		g.child = nil
	}
	expected := job.Version
	if job.Action == "rollback" {
		var manifest backupManifest
		if err := readJSON(filepath.Join(g.service.dir, "backup.json"), &manifest); err != nil {
			return err
		}
		if len(manifest.Entries) == 0 {
			return fmt.Errorf("empty rollback manifest")
		}
		expected = manifest.Entries[0].Binary.Version
	}
	ctx, cancel := context.WithDeadline(g.context, deadline)
	err := g.service.VerifySupervised()
	if err == nil {
		g.child, err = g.launch(g.context, g.binary.Path)
	}
	if err == nil {
		err = g.child.Healthy(ctx, expected)
	}
	cancel()
	if err == nil {
		return g.finish(expected, false, nil)
	}
	cause := fmt.Errorf("updated hub failed its health check: %w", err)
	if g.child != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stopErr := g.child.Stop(ctx)
		cancel()
		if stopErr != nil {
			return g.fail(errors.Join(cause, stopErr))
		}
		g.child = nil
	}
	previous, restoreErr := g.service.RestoreSupervised()
	if restoreErr != nil {
		return g.fail(errors.Join(cause, fmt.Errorf("automatic rollback failed: %w", restoreErr)))
	}
	g.child, err = g.launch(g.context, g.binary.Path)
	if err != nil {
		return g.fail(errors.Join(cause, err))
	}
	ctx, cancel = context.WithTimeout(g.context, g.deadline)
	err = g.child.Healthy(ctx, previous)
	cancel()
	if err != nil {
		return g.fail(errors.Join(cause, fmt.Errorf("restored hub failed health check: %w", err)))
	}
	return g.finish(previous, true, cause)
}
func (g *Guardian) finish(version string, rolledBack bool, cause error) error {
	if err := g.service.FinishSupervised(version, rolledBack, cause); err != nil {
		return err
	}
	if g.outcome != nil {
		g.outcome(*g.service.Pending())
	}
	g.binary.Version = version
	return nil
}
func (g *Guardian) fail(cause error) error {
	if err := g.finish(g.binary.Version, false, cause); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// Wait returns on an unexpected serving-child exit so the host supervisor can
// restart the guardian. Intentional update restarts replace the child under mu.
func (g *Guardian) Wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		child := g.child
		g.mu.Unlock()
		observable, ok := child.(interface{ Done() <-chan struct{} })
		if !ok {
			return fmt.Errorf("serving child has no exit observation")
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-g.fatal:
			return err
		case <-observable.Done():
		}
		g.mu.Lock()
		same := g.child == child
		g.mu.Unlock()
		if same {
			return fmt.Errorf("serving hub exited unexpectedly")
		}
	}
}
