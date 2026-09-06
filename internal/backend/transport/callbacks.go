package transport

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const callbackPrefix = "/v2/provider-callbacks/"
const callbackLimit = 1 << 20

var callbackID = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,128}$`)

// CallbackRegistry is instance-local routing only. It accepts no ordinary
// operator/agent bearer and never constructs native provenance or authority.
type CallbackRegistry struct {
	socket  string
	mu      sync.Mutex
	closed  bool
	entries map[string]*callbackEntry
}
type callbackEntry struct {
	registry     *CallbackRegistry
	registration ports.CallbackRegistration
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	closed       bool
}

func NewCallbackRegistry(socket string) (*CallbackRegistry, error) {
	if !filepath.IsAbs(socket) {
		return nil, errors.New("callback socket must be absolute")
	}
	return &CallbackRegistry{socket: socket, entries: map[string]*callbackEntry{}}, nil
}
func (c *CallbackRegistry) RegisterCallback(ctx context.Context, r ports.CallbackRegistration) (ports.CallbackBinding, error) {
	if err := ctx.Err(); err != nil {
		return ports.CallbackBinding{}, err
	}
	if !callbackID.MatchString(r.RegistrationID) || r.ExecutionID == "" || r.Attempt == 0 || r.Handler == nil || r.CredentialDigest == (ports.CallbackCredentialDigest{}) || r.MaxRequestBytes == 0 || r.MaxRequestBytes > callbackLimit || r.MaxResponseBytes == 0 || r.MaxResponseBytes > callbackLimit {
		return ports.CallbackBinding{}, errors.New("invalid callback registration")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.entries[r.RegistrationID] != nil {
		return ports.CallbackBinding{}, errors.New("callback registration unavailable")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	entry := &callbackEntry{registry: c, registration: r, ctx: lifetime, cancel: cancel}
	c.entries[r.RegistrationID] = entry
	return ports.CallbackBinding{RegistrationID: r.RegistrationID, ExecutionID: r.ExecutionID, Attempt: r.Attempt, Endpoint: c.socket, Route: callbackPrefix + r.RegistrationID, Cleanup: entry}, nil
}
func (e *callbackEntry) RegistrationID() string           { return e.registration.RegistrationID }
func (e *callbackEntry) ExecutionID() model.ExecutionID   { return e.registration.ExecutionID }
func (e *callbackEntry) Attempt() model.AttemptGeneration { return e.registration.Attempt }
func (e *callbackEntry) Close(context.Context) error {
	e.registry.mu.Lock()
	if e.registry.entries[e.RegistrationID()] == e {
		delete(e.registry.entries, e.RegistrationID())
	}
	e.registry.mu.Unlock()
	e.mu.Lock()
	e.closed = true
	e.cancel()
	e.mu.Unlock()
	return nil
}
func (c *CallbackRegistry) Close() {
	c.mu.Lock()
	c.closed = true
	entries := c.entries
	c.entries = map[string]*callbackEntry{}
	c.mu.Unlock()
	for _, e := range entries {
		_ = e.Close(context.Background())
	}
}
func (h *Handler) RegisterCallbackIngress(c *CallbackRegistry) error {
	if c == nil {
		return errors.New("callback registry required")
	}
	h.mux.Handle("POST "+callbackPrefix+"{registration}", c)
	return nil
}
func (c *CallbackRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost || r.Header.Get("Origin") != "" {
		http.Error(w, "callback refused", http.StatusForbidden)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, callbackPrefix)
	c.mu.Lock()
	entry := c.entries[id]
	c.mu.Unlock()
	auth := r.Header.Get("Authorization")
	if entry == nil || len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(auth, "Native ") || len(auth) < 39 || len(auth) > 520 {
		http.Error(w, "callback refused", http.StatusUnauthorized)
		return
	}
	digest := sha256.Sum256([]byte(strings.TrimPrefix(auth, "Native ")))
	if subtle.ConstantTimeCompare(digest[:], entry.registration.CredentialDigest[:]) != 1 {
		http.Error(w, "callback refused", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(entry.ctx, cancel)
	defer stop()
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(2 * time.Minute))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(entry.registration.MaxRequestBytes)))
	if err != nil {
		http.Error(w, "invalid callback body", http.StatusBadRequest)
		return
	}
	sink := &callbackResponse{entry: entry, w: w, ctx: ctx}
	err = entry.registration.Handler.HandleNativeCallback(ctx, ports.RawNativeCallback{Body: body, ReceivedAt: time.Now()}, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.written {
		if err != nil {
			http.Error(w, "callback unavailable", http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

type callbackResponse struct {
	entry   *callbackEntry
	w       http.ResponseWriter
	ctx     context.Context
	mu      sync.Mutex
	written bool
}

func (s *callbackResponse) Respond(ctx context.Context, response ports.RawNativeCallbackResponse) (ports.EffectDisposition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry.mu.Lock()
	defer s.entry.mu.Unlock()
	if s.written || s.entry.closed || ctx.Err() != nil || s.ctx.Err() != nil {
		return ports.EffectRefused, errors.New("callback response no longer available")
	}
	if len(response.Body) > int(s.entry.registration.MaxResponseBytes) || response.StatusCode < 200 || response.StatusCode > 599 || (response.ContentType != "application/json" && response.ContentType != "text/plain") {
		return ports.EffectRefused, errors.New("invalid callback response")
	}
	s.written = true
	controller := http.NewResponseController(s.w)
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := s.ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = controller.SetWriteDeadline(deadline)
	s.w.Header().Set("Content-Type", response.ContentType)
	s.w.WriteHeader(response.StatusCode)
	n, err := s.w.Write(response.Body)
	if err != nil || n != len(response.Body) {
		return ports.EffectUnknown, errors.New("callback write uncertain")
	}
	if err := controller.Flush(); err != nil {
		return ports.EffectUnknown, errors.New("callback flush uncertain")
	}
	return ports.EffectAccepted, nil
}
