package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"golang.org/x/sys/unix"
)

type reviewSlowProvider struct {
	ports.Provider
	entered chan struct{}
	release chan struct{}
	exited  chan struct{}
}

func (*reviewSlowProvider) Name() string { return "review-slow" }
func (*reviewSlowProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{}
}
func (p *reviewSlowProvider) Prepare(ctx context.Context, _ ports.PreparationRequest) (ports.PreparedAttempt, error) {
	close(p.entered)
	defer close(p.exited)
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return nil, errors.New("review preparation complete")
}
func TestShutdownJoinsAdmittedWorkflowBeforeReleasingState(t *testing.T) {
	parent, err := os.MkdirTemp("", "review-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	dir := filepath.Join(parent, "s")
	if err := Initialize(dir); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(dir, "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	p := &reviewSlowProvider{entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, dir, providers.NewRegistry(p)) }()
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "api.sock"))
	}}}
	defer client.CloseIdleConnections()
	for n := 0; n < 100; n++ {
		if _, err := os.Stat(filepath.Join(dir, "api.sock")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		req, _ := http.NewRequest("POST", "http://backend/v2/launch", strings.NewReader(`{"request_id":"slow-launch","target":{"standalone":{"desired":{"Harness":"review-slow","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"unconfined"}}}}`))
		req.Header.Set("Authorization", "Bearer "+string(token))
		res, err := client.Do(req)
		if err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("prepare did not start")
	}
	cancel()
	select {
	case err := <-done:
		close(p.release)
		t.Fatalf("Serve returned while admitted work remained: %v", err)
	case <-time.After(5500 * time.Millisecond):
	}
	select {
	case <-p.exited:
		t.Fatal("workflow unexpectedly exited")
	default:
	}
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		close(p.release)
		t.Fatal("state lock released before admitted workflow settled")
	}

	close(p.release)
	<-p.exited
	<-requestDone
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
