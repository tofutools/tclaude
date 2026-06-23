package remoteaccess

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// clientNameRe constrains a device/client name to a safe filename token. The
// name becomes a path component (clients/<name>.crt / .p12), so this is the
// path-traversal guard now that AddClient is reachable from the localhost
// dashboard, not just the operator's CLI: letters, digits, '-' and '_' only, a
// leading alphanumeric, ≤64 chars — no '/', no '.', so "..", absolute paths and
// hidden files are all impossible.
var clientNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidClientName reports whether name is a safe client-cert filename token.
func ValidClientName(name string) bool { return clientNameRe.MatchString(name) }

// dnsNameRe matches a syntactically valid DNS name (one or more labels of
// alphanumerics with internal hyphens, an optional trailing dot). The stdlib
// has no EXPORTED domain-name validator — the equivalent logic lives only as the
// unexported net.isDomainName — and the only library that does (x/net/idna) is
// overkill for this, so a small local regex is the right call rather than a new
// dependency.
var dnsNameRe = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*\.?$`)

// ValidHost reports whether h is a usable server-cert SAN: an IP address
// (net.ParseIP, stdlib) or a syntactically valid DNS name (dnsNameRe). Guards an
// operator-entered host list so a junk token can't land in the cert as a bogus
// SAN or fail x509 issuance opaquely.
func ValidHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	return dnsNameRe.MatchString(h)
}

// ServerCertSANs returns the host names + IPs the issued server certificate is
// valid for (its SAN list) — the exact set a client can dial without a name
// mismatch. Read from the on-disk cert (not recomputed from the live network),
// so it reflects what setup actually baked in. Errors if no material exists.
func ServerCertSANs() ([]string, error) {
	pemBytes, err := os.ReadFile(serverCertPath())
	if err != nil {
		return nil, fmt.Errorf("read server cert: %w", err)
	}
	cert, err := parseCertPEM(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parse server cert: %w", err)
	}
	sans := append([]string{}, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		sans = append(sans, ip.String())
	}
	return sans, nil
}

// ClientInfo describes one issued device identity (from its recorded public
// cert under clients/<name>.crt). HasP12 reports whether the device's .p12 is
// still on disk and therefore downloadable — a setup writes both, but a .p12
// may be deleted after install while the .crt is kept for the audit record.
type ClientInfo struct {
	Name     string    `json:"name"`
	NotAfter time.Time `json:"not_after"`
	HasP12   bool      `json:"has_p12"`
}

// ListClients returns every issued device identity, sorted by name. An absent
// clients dir (nothing issued yet) is not an error — it yields an empty list.
func ListClients() ([]ClientInfo, error) {
	entries, err := os.ReadDir(clientsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read clients dir: %w", err)
	}
	var out []ClientInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".crt") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".crt")
		ci := ClientInfo{Name: name}
		if pemBytes, err := os.ReadFile(filepath.Join(clientsDir(), e.Name())); err == nil {
			if cert, perr := parseCertPEM(pemBytes); perr == nil {
				ci.NotAfter = cert.NotAfter
			}
		}
		if _, err := os.Stat(filepath.Join(clientsDir(), name+".p12")); err == nil {
			ci.HasP12 = true
		}
		out = append(out, ci)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ClientP12 returns the password-protected .p12 bytes for a device, for the
// dashboard to offer as a download. The name is validated (path guard) and the
// file must exist. The .p12 is served to any authenticated dashboard caller
// (loopback cookie OR a remote mTLS + passphrase session) — a remote session is
// already a full control-plane operator, so handing out a (password-protected)
// device bundle sits at the same privilege tier (operator decision, JOH-278).
func ClientP12(name string) ([]byte, error) {
	if !ValidClientName(name) {
		return nil, fmt.Errorf("invalid client name %q", name)
	}
	data, err := os.ReadFile(filepath.Join(clientsDir(), name+".p12"))
	if err != nil {
		return nil, fmt.Errorf("read .p12 for %q: %w", name, err)
	}
	return data, nil
}

// ReissueServerCert regenerates ONLY the server certificate — signed by the
// EXISTING CA — so it can cover additional host names / IPs (a public URL, a new
// tailnet name) without rotating the CA. This is the non-destructive way to
// "support a new URL": existing client devices keep working untouched, because
// they trust the CA (unchanged) and verify the freshly-issued server cert
// against it. Contrast Setup --regenerate-certs, which rotates the CA and
// invalidates every installed device.
//
// The new SAN set is the union of the cert's CURRENT SANs, the always-included
// local set (localhost / loopback / hostname / local IPs + the bind host), and
// extraHosts — additions are cumulative, so a previously-added name is never
// silently dropped. The running listener keeps serving the old cert until agentd
// restarts. Requires Setup to have run.
func ReissueServerCert(bind string, extraHosts []string) ([]string, error) {
	if !Exists() {
		return nil, fmt.Errorf("remote-access not configured — run `tclaude remote-access setup` first")
	}
	caCertPEM, err := os.ReadFile(caCertPath())
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	caKeyPEM, err := os.ReadFile(caKeyPath())
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}
	existing, _ := ServerCertSANs() // best-effort; empty on a read/parse error
	hosts := dedupeHosts(append(existing, ServerCertHosts(bind, extraHosts)...))
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
	return hosts, nil
}

func dedupeHosts(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, h := range in {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// CACert returns the CA certificate PEM (public material — safe to hand out so
// a device/browser can trust the self-signed server cert). Errors if no
// material exists.
func CACert() ([]byte, error) {
	data, err := os.ReadFile(caCertPath())
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	return data, nil
}
