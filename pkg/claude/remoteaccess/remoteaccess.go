// Package remoteaccess holds the secret material and crypto primitives for
// tclaude's optional network-exposed dashboard listener (the LAN / mesh /
// tunnel "remote access" of the tclaude-remote-control project).
//
// The loopback dashboard is unchanged and keeps its init-token → session
// cookie flow. This package only concerns the SEPARATE remote listener that
// agentd starts when `remote_access.enabled` is set (see
// config.RemoteAccessConfig). That listener is a network-exposed agent control
// plane — it can spawn/kill agents and is a send-keys injection sink — so its
// auth is built to the public-internet bar: every request must satisfy BOTH
//
//   - mTLS: a client certificate issued by the tclaude remote-access CA,
//     enforced at the TLS layer (RequireAndVerifyClientCert), AND
//   - a passphrase login that mints a signed, restart-surviving session cookie.
//
// All material lives as 0600 files under Dir() (~/.tclaude/remote-access/),
// never in config.json. `tclaude remote-access setup` generates it; agentd
// loads it via Load().
package remoteaccess

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/common"
)

// File names under Dir(). The CA key is kept (0600) so `remote-access
// add-client` can issue further client certs; client PRIVATE keys are never
// persisted server-side — they are bundled into the one-time .p12 handed to the
// device, and the server only needs the CA cert to verify them.
const (
	caCertFile     = "ca.crt"
	caKeyFile      = "ca.key"
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"
	cookieKeyFile  = "cookie.key"
	authFile       = "auth.json"
	clientsSubdir  = "clients"
)

// Dir returns the remote-access material directory
// (~/.tclaude/data/remote-access/). It holds CA and server private keys, the
// cookie key, and the auth passphrase hash, so it lives under the private
// data/ subtree that is denied to sandboxed agents.
func Dir() string {
	return common.TclaudeStatePath("remote-access")
}

func caCertPath() string     { return filepath.Join(Dir(), caCertFile) }
func caKeyPath() string      { return filepath.Join(Dir(), caKeyFile) }
func serverCertPath() string { return filepath.Join(Dir(), serverCertFile) }
func serverKeyPath() string  { return filepath.Join(Dir(), serverKeyFile) }
func cookieKeyPath() string  { return filepath.Join(Dir(), cookieKeyFile) }
func authPath() string       { return filepath.Join(Dir(), authFile) }
func clientsDir() string     { return filepath.Join(Dir(), clientsSubdir) }

// authData is the JSON shape of auth.json: the encoded passphrase hash (see
// HashPassphrase). Kept as a tiny struct so a future field (e.g. a revocation
// list) can land without a format break.
type authData struct {
	Passphrase string `json:"passphrase"`
}

// Material is the loaded server-side remote-access secret set: enough to stand
// up the mTLS listener (CA to verify clients + server keypair), verify the
// passphrase, and sign/verify session cookies. Client private keys are NOT
// part of it — the server never holds them.
type Material struct {
	caCert     *x509.Certificate // verifies presented client certs
	caCertPEM  []byte            // PEM, for bundling into client .p12s
	serverCert tls.Certificate   // the listener's TLS keypair
	cookieKey  []byte            // HMAC key for signed session cookies
	passHash   string            // encoded passphrase hash (pbkdf2)
}

// Exists reports whether remote-access material has been generated (a setup has
// run). It is a cheap presence check on the CA cert, used to give a clear "run
// `tclaude remote-access setup` first" error rather than a load failure.
func Exists() bool {
	_, err := os.Stat(caCertPath())
	return err == nil
}

// Load reads the full server-side material from Dir(). It returns a clear
// error when nothing has been generated yet (Exists() is false) so agentd can
// refuse to start the remote listener with an actionable message rather than a
// cryptic file-not-found.
func Load() (*Material, error) {
	if !Exists() {
		return nil, fmt.Errorf("remote-access material not found in %s — run `tclaude remote-access setup` first", Dir())
	}

	caCertPEM, err := os.ReadFile(caCertPath())
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	caCert, err := parseCertPEM(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	serverCert, err := tls.LoadX509KeyPair(serverCertPath(), serverKeyPath())
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}

	cookieKey, err := os.ReadFile(cookieKeyPath())
	if err != nil {
		return nil, fmt.Errorf("read cookie key: %w", err)
	}
	if len(cookieKey) < 32 {
		return nil, fmt.Errorf("cookie key in %s is too short (%d bytes); re-run `tclaude remote-access setup`", cookieKeyPath(), len(cookieKey))
	}

	authRaw, err := os.ReadFile(authPath())
	if err != nil {
		return nil, fmt.Errorf("read auth file: %w", err)
	}
	var auth authData
	if err := json.Unmarshal(authRaw, &auth); err != nil {
		return nil, fmt.Errorf("parse auth file: %w", err)
	}
	if auth.Passphrase == "" {
		return nil, fmt.Errorf("no passphrase set in %s; re-run `tclaude remote-access setup`", authPath())
	}

	return &Material{
		caCert:     caCert,
		caCertPEM:  caCertPEM,
		serverCert: serverCert,
		cookieKey:  cookieKey,
		passHash:   auth.Passphrase,
	}, nil
}

// TLSConfig returns the listener TLS config that enforces mTLS: it presents the
// server cert and REQUIRES a client certificate that verifies against the
// tclaude CA. A connection without a valid client cert is rejected during the
// TLS handshake, before any HTTP handler runs — the first of the two auth
// factors (the passphrase login is the second).
func (m *Material) TLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(m.caCert)
	return &tls.Config{
		Certificates: []tls.Certificate{m.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}
}

// CookieKey returns the HMAC key for signing/verifying session cookies. agentd
// passes it to SignCookie / VerifyCookie for the remote listener.
func (m *Material) CookieKey() []byte { return m.cookieKey }

// VerifyPassphrase reports whether pw matches the stored passphrase hash, in
// constant time (see VerifyPassphraseHash).
func (m *Material) VerifyPassphrase(pw string) bool {
	return VerifyPassphraseHash(m.passHash, pw)
}

// parseCertPEM decodes the first CERTIFICATE block from PEM bytes.
func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}
