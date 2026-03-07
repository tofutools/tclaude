package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certDir returns ~/.tofu/claude-web/
func certDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tofu", "claude-web"), nil
}

// deleteCerts removes existing cert and key files, forcing regeneration on next start.
func deleteCerts() error {
	dir, err := certDir()
	if err != nil {
		return err
	}
	os.Remove(filepath.Join(dir, "cert.pem"))
	os.Remove(filepath.Join(dir, "key.pem"))
	return nil
}

// loadOrGenerateCert loads an existing cert from disk, or generates a new one
// and saves it. Returns the TLS config and SHA-256 fingerprint.
func loadOrGenerateCert() (*tls.Config, string, error) {
	dir, err := certDir()
	if err != nil {
		return nil, "", err
	}

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	// Try loading existing cert
	if tlsConfig, fp, err := loadCert(certPath, keyPath); err == nil {
		return tlsConfig, fp, nil
	}

	// Generate new cert and save
	fmt.Println("  generating new TLS certificate...")
	return generateAndSaveCert(dir, certPath, keyPath)
}

// loadCert loads and validates an existing certificate from disk.
func loadCert(certPath, keyPath string) (*tls.Config, string, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, "", err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, "", err
	}

	// Check expiry
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, "", fmt.Errorf("invalid cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", err
	}
	if time.Now().After(cert.NotAfter) {
		return nil, "", fmt.Errorf("certificate expired")
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, "", err
	}

	fingerprint := sha256.Sum256(cert.Raw)
	fpStr := fmt.Sprintf("%X", fingerprint)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	}
	return tlsConfig, fpStr, nil
}

// generateAndSaveCert generates a self-signed cert, saves it to disk, and returns the TLS config.
func generateAndSaveCert(dir, certPath, keyPath string) (*tls.Config, string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, "", fmt.Errorf("create cert dir: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, "", fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tofu claude web"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),

		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,

		DNSNames:    []string{"localhost"},
		IPAddresses: localIPs(),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, "", fmt.Errorf("create certificate: %w", err)
	}

	// Save cert
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return nil, "", fmt.Errorf("save cert: %w", err)
	}

	// Save key
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, "", fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, "", fmt.Errorf("save key: %w", err)
	}

	fmt.Printf("  cert saved to: %s\n", certPath)
	fmt.Printf("  key saved to:  %s\n", keyPath)

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, "", fmt.Errorf("load keypair: %w", err)
	}

	fingerprint := sha256.Sum256(certDER)
	fpStr := fmt.Sprintf("%X", fingerprint)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	}
	return tlsConfig, fpStr, nil
}

// localIPs returns all non-loopback IPv4 addresses on the machine
func localIPs() []net.IP {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				ips = append(ips, ipnet.IP)
			}
		}
	}
	return ips
}
