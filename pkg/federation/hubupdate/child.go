package hubupdate

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Child owns exactly one serving process. Readiness travels over the private
// generation-specific channel before probing the listener it actually bound.
type Child struct {
	cmd      *exec.Cmd
	lifetime *os.File
	ready    <-chan Ready
	done     chan struct{}
	once     sync.Once
	err      error
	hubID    string
	tls      bool
}

func Launch(ctx context.Context, bridge *Bridge, binary string, args []string, hubID string, useTLS bool) (*Child, error) {
	config, ready, err := bridge.NewWorker()
	if err != nil {
		return nil, err
	}
	input, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer input.Close()
	life, keep, err := os.Pipe()
	if err != nil {
		writer.Close()
		return nil, err
	}
	defer life.Close()
	c := &Child{cmd: exec.Command(binary, args...), lifetime: keep, ready: ready, done: make(chan struct{}), hubID: hubID, tls: useTLS}
	c.cmd.ExtraFiles = []*os.File{input, life}
	c.cmd.Stdout = os.Stdout
	c.cmd.Stderr = os.Stderr
	if err = c.cmd.Start(); err != nil {
		writer.Close()
		keep.Close()
		return nil, err
	}
	go func() { c.err = c.cmd.Wait(); close(c.done) }()
	err = json.NewEncoder(writer).Encode(config)
	writer.Close()
	if err != nil {
		_ = c.Stop(ctx)
		return nil, err
	}
	return c, nil
}
func (c *Child) Done() <-chan struct{} { return c.done }
func (c *Child) Stop(ctx context.Context) error {
	c.once.Do(func() { _ = c.lifetime.Close(); _ = c.cmd.Process.Signal(syscall.SIGTERM) })
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		_ = c.cmd.Process.Kill()
		select {
		case <-c.done:
			return nil
		case <-time.After(time.Second):
			return fmt.Errorf("serving hub did not exit after kill")
		}
	}
}
func (c *Child) Healthy(ctx context.Context, version string) error {
	var ready Ready
	select {
	case ready = <-c.ready:
	case <-c.done:
		return fmt.Errorf("serving hub exited before readiness: %v", c.err)
	case <-ctx.Done():
		return ctx.Err()
	}
	if ready.HubID != c.hubID || ready.Version != version {
		return fmt.Errorf("serving hub identity/version mismatch")
	}
	host, port, err := net.SplitHostPort(ready.Address)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	scheme := "http"
	if c.tls {
		scheme = "https"
	}
	// The authenticated private readiness channel pins this exact local listener
	// and its expected identity/version. A public PKI name may not cover loopback.
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, Proxy: nil}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "GET", scheme+"://"+net.JoinHostPort(host, port)+"/healthz", nil)
	if err != nil {
		return err
	}
	res, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var value struct {
		OK      bool   `json:"ok"`
		HubID   string `json:"hub_id"`
		Version string `json:"version"`
	}
	if res.StatusCode != 200 || json.NewDecoder(http.MaxBytesReader(nil, res.Body, 4096)).Decode(&value) != nil || !value.OK || value.HubID != c.hubID || value.Version != version {
		return fmt.Errorf("serving hub health identity/version mismatch")
	}
	select {
	case <-c.done:
		return fmt.Errorf("serving hub exited during health check: %v", c.err)
	default:
		return nil
	}
}
func WorkerArgs(args []string) []string {
	// All original host settings are preserved; only guardian mode is overridden.
	return append(append([]string(nil), args...), "--supervised=false", "--guardian-worker=true")
}
