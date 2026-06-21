package remoteaccess

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// Validity windows. The CA outlives the leaves it signs so a server/client
// re-issue doesn't force a CA rotation (which would invalidate every device's
// installed client cert).
const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 2 * 365 * 24 * time.Hour
)

// newKey generates a fresh P-256 ECDSA key — same curve the deprecated web
// terminal used, small and universally supported by browsers + mobile.
func newKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// encodeKeyPEM marshals an ECDSA key as a PKCS#8 "PRIVATE KEY" PEM — the
// universal form that tls.LoadX509KeyPair and pkcs12 both accept.
func encodeKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func encodeCertPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// parseKeyPEM decodes a PKCS#8 "PRIVATE KEY" PEM into an ECDSA key.
func parseKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not ECDSA")
	}
	return ec, nil
}

// GenerateCA creates a self-signed CA used to sign the server cert and every
// client cert. Returns cert + key PEM. The CA's private key must be kept (0600)
// so `remote-access add-client` can issue further client certs.
func GenerateCA() (certPEM, keyPEM []byte, err error) {
	key, err := newKey()
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tclaude remote-access CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // signs leaves only, no sub-CAs
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA cert: %w", err)
	}
	keyPEM, err = encodeKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return encodeCertPEM(der), keyPEM, nil
}

// loadCA parses a CA cert+key PEM pair into the values x509.CreateCertificate
// needs as the issuer.
func loadCA(caCertPEM, caKeyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	caCert, err := parseCertPEM(caCertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}
	caKey, err := parseKeyPEM(caKeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	return caCert, caKey, nil
}

// GenerateServerCert issues a server (ServerAuth) cert signed by the CA, valid
// for the given hosts (the SAN list — IPs go in IPAddresses, names in
// DNSNames). The phone must reach the listener at a name/IP present here, so
// the caller assembles hosts from the bind address + local IPs + hostname +
// any operator-supplied extras (see ServerCertHosts).
func GenerateServerCert(caCertPEM, caKeyPEM []byte, hosts []string) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := loadCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	key, err := newKey()
	if err != nil {
		return nil, nil, fmt.Errorf("generate server key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tclaude remote-access server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create server cert: %w", err)
	}
	keyPEM, err = encodeKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return encodeCertPEM(der), keyPEM, nil
}

// GenerateClientCert issues a client (ClientAuth) cert signed by the CA, with
// name as the CommonName so issued devices are distinguishable (and a future
// revocation list can key on it). Returns cert + key PEM; the caller bundles
// them into a .p12 (ExportPKCS12) for the device and persists only the cert.
func GenerateClientCert(caCertPEM, caKeyPEM []byte, name string) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := loadCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	key, err := newKey()
	if err != nil {
		return nil, nil, fmt.Errorf("generate client key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create client cert: %w", err)
	}
	keyPEM, err = encodeKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return encodeCertPEM(der), keyPEM, nil
}

// ExportPKCS12 bundles a client cert + key (+ the CA cert, so the device can
// also verify the server) into password-protected PKCS#12 bytes — the .p12 an
// operator installs on the phone as a client identity. Uses the modern
// PKCS#12 encryption profile (compatible with current iOS 16.4+/Android); very
// old devices may need a legacy profile, noted in the docs.
func ExportPKCS12(clientCertPEM, clientKeyPEM, caCertPEM []byte, password string) ([]byte, error) {
	clientCert, err := parseCertPEM(clientCertPEM)
	if err != nil {
		return nil, fmt.Errorf("parse client cert: %w", err)
	}
	clientKey, err := parseKeyPEM(clientKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse client key: %w", err)
	}
	caCert, err := parseCertPEM(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	return pkcs12.Modern.Encode(clientKey, clientCert, []*x509.Certificate{caCert}, password)
}

// ServerCertHosts assembles the SAN list for the server cert: localhost +
// loopback, the machine hostname, every non-loopback local IPv4, the bind
// host (when it names a concrete address rather than a wildcard), plus any
// operator-supplied extras (a tailnet name, a tunnel hostname). Deduplicated,
// order-stable.
func ServerCertHosts(bind string, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}

	add("localhost")
	add("127.0.0.1")
	add("::1")
	if hn, err := os.Hostname(); err == nil {
		add(hn)
	}
	for _, ip := range localIPv4s() {
		add(ip)
	}
	// The bind host, when it's a concrete address (not 0.0.0.0 / empty / a
	// bare port). A wildcard bind contributes no usable SAN.
	if host, _, err := net.SplitHostPort(bind); err == nil {
		if host != "" && host != "0.0.0.0" && host != "::" {
			add(host)
		}
	}
	for _, h := range extra {
		add(h)
	}
	return out
}

// localIPv4s returns this machine's non-loopback IPv4 addresses, so the LAN
// preset's cert is valid for whatever address the phone dials.
func localIPv4s() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if v4 := ipnet.IP.To4(); v4 != nil {
				out = append(out, v4.String())
			}
		}
	}
	return out
}
