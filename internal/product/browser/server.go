// Package browser serves the product UI as an authenticated local client of the
// Unix API. It never opens backend storage or resolves execution authority.
package browser

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed assets/*
var assets embed.FS

type Server struct {
	cookieName     string
	listener       net.Listener
	origin         string
	bootstrap      string
	bootstrapUntil time.Time
	mu             sync.Mutex
	redeemed       bool
	session        string
	sessionUntil   time.Time
	state          string
	proxy          *httputil.ReverseProxy
	transport      *http.Transport
	lifetime       context.Context
	cancel         context.CancelFunc
	stopping       bool
	active         sync.WaitGroup
}

// Open requires an explicit loopback listener and initialized backend directory.
// The returned bootstrap URL is private, short-lived and redeemable once.
func Open(state, address string) (*Server, error) {
	if !filepath.IsAbs(state) {
		return nil, errors.New("operator state directory must be absolute")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, errors.New("browser listener must be an explicit loopback IP and port")
	}
	if _, err := operatorToken(state); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		_ = listener.Close()
		return nil, err
	}
	cookieID := make([]byte, 16)
	if _, err := rand.Read(cookieID); err != nil {
		_ = listener.Close()
		return nil, err
	}
	s := &Server{cookieName: "tclaude_browser_" + hex.EncodeToString(cookieID), listener: listener, origin: "http://" + listener.Addr().String(), bootstrap: hex.EncodeToString(token), bootstrapUntil: time.Now().Add(5 * time.Minute), state: state}
	s.lifetime, s.cancel = context.WithCancel(context.Background())
	s.transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(state, "api.sock"))
	}}
	s.proxy = &httputil.ReverseProxy{Transport: s.transport, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(&url.URL{Scheme: "http", Host: "backend"})
		pr.Out.Header.Del("Cookie")
		pr.Out.Header.Del("Origin") // Validated locally before the authenticated Unix hop.
		pr.Out.Header.Del("Authorization")
		if token, ok := pr.In.Context().Value(tokenKey{}).(string); ok {
			pr.Out.Header.Set("Authorization", "Bearer "+token)
		}
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
	}}
	return s, nil
}

type tokenKey struct{}

func (s *Server) URL() string { return s.origin + "/#login=" + s.bootstrap }
func (s *Server) Close() error {
	s.mu.Lock()
	s.stopping = true
	s.cancel()
	s.mu.Unlock()
	err := s.listener.Close()
	s.active.Wait()
	s.transport.CloseIdleConnections()
	return err
}

func (s *Server) Serve(ctx context.Context) error {
	defer func() { _ = s.Close() }()
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return s.lifetime }}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(s.listener) }()
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.cancel()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		err := <-finished
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		http.Error(w, "browser stopping", http.StatusServiceUnavailable)
		return
	}
	s.active.Add(1)
	s.mu.Unlock()
	defer s.active.Done()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if "http://"+r.Host != s.origin {
		http.Error(w, "unexpected host", http.StatusForbidden)
		return
	}
	if values := r.Header.Values("Origin"); len(values) > 1 || (len(values) == 1 && values[0] != s.origin) {
		http.Error(w, "foreign origin", http.StatusForbidden)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "foreign site", http.StatusForbidden)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != s.origin {
		http.Error(w, "origin required", http.StatusForbidden)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && r.Header.Get("Origin") != s.origin {
		http.Error(w, "origin required", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/session" && r.Method == "POST" {
		s.login(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v2/") {
		if !s.authenticated(r) {
			http.Error(w, "browser login required", http.StatusUnauthorized)
			return
		}
		token, err := operatorToken(s.state)
		if err != nil {
			http.Error(w, "operator credential unavailable", http.StatusServiceUnavailable)
			return
		}
		s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, token)))
		return
	}
	if r.URL.Path == "/session" && r.Method == "DELETE" {
		if !s.authenticated(r) {
			http.Error(w, "browser login required", http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		s.session = ""
		s.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: s.cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	files, _ := fs.Sub(assets, "assets")
	http.FileServer(http.FS(files)).ServeHTTP(w, r)
}

func (s *Server) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(s.cookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session != "" && time.Now().Before(s.sessionUntil) && equal(cookie.Value, s.session)
}
func equal(a, b string) bool {
	aa, bb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aa[:], bb[:]) == 1
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "invalid login", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "invalid login", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.redeemed || time.Now().After(s.bootstrapUntil) || !equal(body.Token, s.bootstrap) {
		http.Error(w, "login link expired or used", http.StatusUnauthorized)
		return
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		http.Error(w, "login unavailable", http.StatusServiceUnavailable)
		return
	}
	s.session = hex.EncodeToString(token)
	s.sessionUntil = time.Now().Add(12 * time.Hour)
	s.redeemed = true
	http.SetCookie(w, &http.Cookie{Name: s.cookieName, Value: s.session, Path: "/", Expires: s.sessionUntil, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}
func operatorToken(state string) (string, error) {
	file, err := os.Open(filepath.Join(state, "operator.token"))
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", err
	}
	if len(data) < 32 || len(data) > 4096 || strings.ContainsAny(string(data), " \r\n\t") {
		return "", errors.New("invalid operator credential resource")
	}
	return string(data), nil
}
