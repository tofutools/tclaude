package hubcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/tofutools/tclaude/pkg/federation/hubupdate"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"golang.org/x/sys/unix"
)

func serveSupervised(p *serveParams) error {
	supervisor, err := hubupdate.DetectSupervisor(p.SupervisorLabel)
	if err != nil {
		return fmt.Errorf("supervised hub unavailable: %w", err)
	}
	st, err := p.open()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	id, err := st.HubID()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err := selfupdate.DiscoverBinary("tclaude-hub", exe)
	if err != nil {
		return err
	}
	dir := p.DB
	if dir == "" {
		dir = DefaultDBPath()
	}
	dir = filepath.Join(filepath.Dir(dir), "hub-update")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	bridge, err := hubupdate.NewBridge(dir)
	if err != nil {
		return err
	}
	defer bridge.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	outcome := func(j selfupdate.Job) {
		if err := st.AuditUpdateOutcome(j); err != nil {
			fmt.Fprintf(os.Stderr, "hub update outcome audit: %v\n", err)
		}
	}
	g, err := selfupdate.NewGuardian(ctx, dir, binary, func(ctx context.Context, path string) (selfupdate.SupervisedChild, error) {
		return hubupdate.Launch(ctx, bridge, path, hubupdate.WorkerArgs(os.Args[1:]), id, p.TLSCert != "")
	}, outcome, st.AuditUpdateProgress)
	if err != nil {
		return err
	}
	defer g.Close()
	bridge.Bind(g.Service(), supervisor, func(actor string) bool { return ctx.Err() == nil && st.ActiveAdminCapability(actor, "hub.update") }, st.AuditUpdateOutcome)
	if j := g.Service().Pending(); j != nil && j.State != "running" && j.State != "restarting" {
		if err := st.AuditUpdateOutcome(*j); err != nil {
			return err
		}
	}
	if err = g.Start(); err != nil {
		return err
	}
	return g.Wait(ctx)
}

func workerContext() (context.Context, context.CancelFunc, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	lifetime := os.NewFile(4, "guardian-lifetime")
	if lifetime == nil {
		cancel()
		return nil, nil, fmt.Errorf("missing guardian lifetime pipe")
	}
	info, err := lifetime.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		lifetime.Close()
		cancel()
		return nil, nil, fmt.Errorf("invalid guardian lifetime pipe")
	}
	unix.CloseOnExec(4)
	go func() { _, _ = io.Copy(io.Discard, lifetime); cancel(); _ = lifetime.Close() }()
	return ctx, cancel, nil
}
