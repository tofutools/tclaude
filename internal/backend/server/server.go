// Package server composes the replacement application and transport. It never
// opens the legacy database or imports the legacy daemon.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"github.com/tofutools/tclaude/internal/backend/transport"
	"golang.org/x/sys/unix"
)

const marker = "tclaude replacement backend development state v1\n"

// Initialize requires a new directory: existing data is never adopted implicitly.
func Initialize(dir string) error {
	return initialize(dir, marker)
}

func initialize(dir, format string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("state directory must be absolute")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("create new state directory: %w", err)
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "operator.token"), []byte(hex.EncodeToString(token[:])), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "FORMAT"), []byte(format), 0600)
}

// JourneyServices are composition-owned resources; requests never select host
// executables, provider roots, or native credentials.
type JourneyServices struct {
	Workspaces  ports.WorkspaceHost
	Shells      ports.ShellHost
	History     ports.HistorySourceRegistry
	Programs    ports.ProgramHost
	FactSources []ports.AutomationFactSource
}

// Serve holds a single-process lock and leaves durable executions recoverable on
// shutdown. Disconnecting clients or stopping this HTTP server does not stop work.
func Serve(ctx context.Context, dir string, registry ports.ProviderRegistry, journey ...JourneyServices) error {
	if len(journey) > 1 {
		return errors.New("only one journey service configuration is allowed")
	}
	if !filepath.IsAbs(dir) {
		return errors.New("state directory must be absolute")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("state directory must be private and must not be a symlink")
	}
	format, err := os.ReadFile(filepath.Join(dir, "FORMAT"))
	if err != nil || string(format) != marker {
		return errors.New("state directory was not initialized for the replacement backend")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("replacement backend is already running for this state directory")
	}
	// Closing lock releases the advisory lock after all server resources close.
	credential, err := os.ReadFile(filepath.Join(dir, "operator.token"))
	if err != nil {
		return err
	}
	auth, err := transport.NewOperatorToken(string(credential))
	if err != nil {
		return err
	}
	store, err := sqlite.Open(filepath.Join(dir, "backend.sqlite"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	socket, err := prepareAPIEndpoint(dir)
	if err != nil {
		return err
	}
	callbacks, err := transport.NewCallbackRegistry(socket)
	if err != nil {
		return err
	}
	defer callbacks.Close()
	sandboxPaths, err := host.NewSandboxPathInspector([]string{dir})
	if err != nil {
		return err
	}
	application := app.New(store, registry).WithDirectoryBrowser(host.DirectoryBrowser{}).WithDirectoryDefaults(host.DirectoryBrowser{}).WithDirectoryWriteProof(host.DirectoryProof{}).WithSandboxPathInspector(sandboxPaths).WithAgentAPIEndpoint(socket).WithCallbackIngress(callbacks)
	if len(journey) == 1 {
		services := journey[0]
		application.WithWorkspaceHost(services.Workspaces).WithShellHost(services.Shells).WithHistorySources(services.History).WithProgramHost(services.Programs).WithAutomationFactSources(services.FactSources...)
	}
	if _, err := application.Recover(ctx, app.RecoverRequest{Principal: model.OperatorPrincipal()}); err != nil {
		return fmt.Errorf("recover backend: %w", err)
	}
	callers, err := transport.NewCallerAuthenticator(auth, application)
	if err != nil {
		return err
	}
	handler, err := transport.NewHandler(application, callers)
	if err != nil {
		return err
	}
	if err := handler.RegisterAgentAPI(application, application); err != nil {
		return err
	}
	if err := handler.RegisterCallbackIngress(callbacks); err != nil {
		return err
	}
	if err := handler.RegisterJourneyAPI(application); err != nil {
		return err
	}
	if err := handler.RegisterOrchestrationAPI(application); err != nil {
		return err
	}
	if err := handler.RegisterAccessRequestAPI(application); err != nil {
		return err
	}
	// Holding the state-directory lock makes this a stale socket from our own
	// previous process. Refuse other file types rather than deleting arbitrary data.
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("API socket path contains a non-socket file")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	stopRenewal := startAccessRenewal(ctx, application)
	defer stopRenewal() // Join before the deferred store close and lock release.
	stopNotifications := startMessageNotifications(ctx, application)
	defer stopNotifications() // Join native notice settlement before store/lock release.
	stopWork := startWorkReconciliation(ctx, application)
	defer stopWork() // Join application work before closing its store.
	requests := &requestDrain{handler: handler}
	requestCtx, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	server := &http.Server{Handler: requests, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		BaseContext: func(net.Listener) context.Context { return requestCtx }}
	completed := make(chan error, 1)
	go func() { completed <- server.Serve(listener) }()
	var serveErr error
	select {
	case serveErr = <-completed:
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
	case <-ctx.Done():
	}
	requests.stopAccepting()
	cancelRequests() // Disconnect views; admitted application work owns its lifetime.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close() // Unblock stalled network I/O, then join the handlers below.
	}
	requests.wait()
	return serveErr
}

// The gate prevents additions racing with Wait, including connections accepted
// just before shutdown whose handler has not started yet.
type requestDrain struct {
	handler  http.Handler
	mu       sync.Mutex
	stopping bool
	active   sync.WaitGroup
}

func (d *requestDrain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		http.Error(w, "server stopping", http.StatusServiceUnavailable)
		return
	}
	d.active.Add(1)
	d.mu.Unlock()
	defer d.active.Done()
	d.handler.ServeHTTP(w, r)
}
func (d *requestDrain) stopAccepting() { d.mu.Lock(); d.stopping = true; d.mu.Unlock() }
func (d *requestDrain) wait()          { d.active.Wait() }
