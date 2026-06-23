package remoteaccess

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SetupOptions drives a first-time (or --regenerate-certs re-) initialisation
// of the remote-access material.
type SetupOptions struct {
	Bind            string   // listen address the server cert must be valid for
	ExtraHosts      []string // additional SANs (tailnet name, tunnel hostname)
	Passphrase      string   // the login passphrase (hashed, never stored plain)
	ClientName      string   // name for the first device's client cert
	P12Password     string   // password protecting the exported .p12
	P12Out          string   // where to write the .p12 (default: clients/<name>.p12)
	RegenerateCerts bool     // regenerate (clobber) even if material already exists
}

// SetupResult reports what Setup produced, for the CLI to print.
type SetupResult struct {
	Dir        string
	Hosts      []string
	ClientName string
	P12Path    string
	CACertPath string
}

// Setup performs a full first-time initialisation: generate the CA, a server
// cert valid for the resolved host set, a random cookie-signing key, store the
// passphrase hash, and issue the first device's client cert + .p12.
//
// By default it REFUSES to clobber existing material — regenerating the CA
// invalidates every client cert already installed on a device — and returns a
// clear error unless RegenerateCerts is set. To add another device without
// rotating the CA, use AddClient. Material is written under Dir() with 0700
// dir / 0600 secrets.
func Setup(o SetupOptions) (*SetupResult, error) {
	if o.Passphrase == "" {
		return nil, fmt.Errorf("a passphrase is required")
	}
	if o.ClientName == "" {
		o.ClientName = "phone"
	}
	if Exists() && !o.RegenerateCerts {
		return nil, fmt.Errorf("remote-access already configured in %s — pass --regenerate-certs to rotate everything (this INVALIDATES installed client certs), or `tclaude remote-access add-client` to add a device", Dir())
	}
	if err := ensureDirs(); err != nil {
		return nil, err
	}

	caCertPEM, caKeyPEM, err := GenerateCA()
	if err != nil {
		return nil, err
	}
	if err := writeSecret(caKeyPath(), caKeyPEM); err != nil {
		return nil, err
	}
	if err := writePublic(caCertPath(), caCertPEM); err != nil {
		return nil, err
	}

	hosts := ServerCertHosts(o.Bind, o.ExtraHosts)
	serverCertPEM, serverKeyPEM, err := GenerateServerCert(caCertPEM, caKeyPEM, hosts)
	if err != nil {
		return nil, err
	}
	if err := writePublic(serverCertPath(), serverCertPEM); err != nil {
		return nil, err
	}
	if err := writeSecret(serverKeyPath(), serverKeyPEM); err != nil {
		return nil, err
	}

	cookieKey := make([]byte, 32)
	if _, err := rand.Read(cookieKey); err != nil {
		return nil, err
	}
	if err := writeSecret(cookieKeyPath(), cookieKey); err != nil {
		return nil, err
	}

	hash, err := HashPassphrase(o.Passphrase)
	if err != nil {
		return nil, err
	}
	if err := writeAuth(authData{Passphrase: hash}); err != nil {
		return nil, err
	}

	client, err := AddClient(o.ClientName, o.P12Password, o.P12Out)
	if err != nil {
		return nil, err
	}

	return &SetupResult{
		Dir:        Dir(),
		Hosts:      hosts,
		ClientName: o.ClientName,
		P12Path:    client.P12Path,
		CACertPath: caCertPath(),
	}, nil
}

// ClientResult reports an issued device identity.
type ClientResult struct {
	Name    string
	P12Path string
}

// AddClient issues a new device client cert from the existing CA and writes its
// password-protected .p12 to p12Out (default clients/<name>.p12). The client
// PRIVATE key is only ever in that .p12 — the server keeps the public cert
// (for the record) but never the key. Requires Setup to have run.
func AddClient(name, p12Password, p12Out string) (*ClientResult, error) {
	// The name becomes a path component (clients/<name>.crt / .p12), so it is
	// charset-validated — a path-traversal guard now that AddClient is reachable
	// from the localhost dashboard, not only the operator's own CLI.
	if !ValidClientName(name) {
		return nil, fmt.Errorf("invalid client name %q: use letters, digits, '-' or '_' (1–64 chars, leading alphanumeric)", name)
	}
	if p12Password == "" {
		return nil, fmt.Errorf("a .p12 password is required (it protects the client key in transit to the device)")
	}
	if !Exists() {
		return nil, fmt.Errorf("remote-access not configured — run `tclaude remote-access setup` first")
	}
	if err := ensureDirs(); err != nil {
		return nil, err
	}
	caCertPEM, err := os.ReadFile(caCertPath())
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	caKeyPEM, err := os.ReadFile(caKeyPath())
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}

	clientCertPEM, clientKeyPEM, err := GenerateClientCert(caCertPEM, caKeyPEM, name)
	if err != nil {
		return nil, err
	}
	// Record the public client cert (not the key) so devices are auditable.
	if err := writePublic(filepath.Join(clientsDir(), name+".crt"), clientCertPEM); err != nil {
		return nil, err
	}

	pfx, err := ExportPKCS12(clientCertPEM, clientKeyPEM, caCertPEM, p12Password)
	if err != nil {
		return nil, err
	}
	if p12Out == "" {
		p12Out = filepath.Join(clientsDir(), name+".p12")
	}
	if err := writeSecret(p12Out, pfx); err != nil {
		return nil, fmt.Errorf("write .p12: %w", err)
	}
	return &ClientResult{Name: name, P12Path: p12Out}, nil
}

func ensureDirs() error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	return os.MkdirAll(clientsDir(), 0o700)
}

// writeSecret writes private material 0600. writePublic writes 0644 material
// (certs are public). Both write atomically via a temp file + rename so a crash
// mid-write can't leave half a key.
func writeSecret(path string, data []byte) error { return writeAtomic(path, data, 0o600) }
func writePublic(path string, data []byte) error { return writeAtomic(path, data, 0o644) }

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func writeAuth(a authData) error {
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeSecret(authPath(), data)
}
