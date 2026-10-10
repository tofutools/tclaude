package hubupdate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type WorkerConfig struct {
	Socket string `json:"socket"`
	Token  string `json:"token"`
}
type Ready struct {
	HubID   string `json:"hub_id"`
	Version string `json:"version"`
	Address string `json:"address"`
}
type Bridge struct {
	mu               sync.Mutex
	path, dir, token string
	ready            chan Ready
	server           *http.Server
	controller       http.Handler
}

func NewBridge(dir string) (*Bridge, error) {
	private, err := os.MkdirTemp(dir, "guardian-")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(private, "control.sock")
	// Unix socket addresses are capped around 104 bytes on macOS. Keep long
	// operator-selected database paths from disabling the private channel.
	if len(path) >= 100 {
		_ = os.RemoveAll(private)
		private, err = os.MkdirTemp("/tmp", "tcl-hub-guardian-")
		if err != nil {
			return nil, err
		}
		path = filepath.Join(private, "control.sock")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(private)
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(private)
		return nil, err
	}
	b := &Bridge{path: path, dir: private}
	b.server = &http.Server{Handler: b, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 5 * time.Second}
	go func() { _ = b.server.Serve(listener) }()
	return b, nil
}
func (b *Bridge) Close() { _ = b.server.Close(); _ = os.RemoveAll(b.dir) }
func (b *Bridge) Bind(s *selfupdate.Service, supervisor string, authorize func(string) bool, audits ...func(selfupdate.Job) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.controller = Handler(s, supervisor, authorize, audits...)
}
func (b *Bridge) NewWorker() (WorkerConfig, <-chan Ready, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return WorkerConfig{}, nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = hex.EncodeToString(nonce[:])
	b.ready = make(chan Ready, 1)
	return WorkerConfig{Socket: b.path, Token: b.token}, b.ready, nil
}
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	token, ready, controller := b.token, b.ready, b.controller
	b.mu.Unlock()
	supplied := r.Header.Get("X-Hub-Guardian-Token")
	if token == "" || len(supplied) != len(token) || subtle.ConstantTimeCompare([]byte(token), []byte(supplied)) != 1 {
		http.Error(w, "private guardian channel", 403)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/ready" {
		var value Ready
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if d.Decode(&value) != nil {
			http.Error(w, "invalid readiness", 400)
			return
		}
		select {
		case ready <- value:
			w.WriteHeader(200)
		default:
			http.Error(w, "duplicate readiness", 409)
		}
		return
	}
	if controller == nil {
		http.Error(w, "guardian initializing", 503)
		return
	}
	controller.ServeHTTP(w, r)
}
func ReadWorkerConfig() (WorkerConfig, error) {
	file := os.NewFile(3, "guardian-config")
	if file == nil {
		return WorkerConfig{}, fmt.Errorf("missing guardian pipe")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return WorkerConfig{}, fmt.Errorf("missing private guardian pipe")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(raw) > 4096 {
		return WorkerConfig{}, fmt.Errorf("invalid guardian config")
	}
	var config WorkerConfig
	if json.Unmarshal(raw, &config) != nil || len(config.Token) != 64 || config.Socket == "" {
		return WorkerConfig{}, fmt.Errorf("invalid guardian config")
	}
	return config, nil
}
func (c Client) Ready(ctx context.Context, value Ready) error {
	raw, _ := json.Marshal(value)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://guardian/ready", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("X-Hub-Guardian-Token", c.Token)
	result, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = result.Body.Close() }()
	if result.StatusCode != 200 {
		return fmt.Errorf("guardian rejected readiness")
	}
	return nil
}
