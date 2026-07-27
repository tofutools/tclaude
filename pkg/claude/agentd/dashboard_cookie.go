package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const dashboardCookieNamespaceFile = "dashboard_cookie_namespace"

// loadOrCreateDashboardCookieName returns a stable, installation-specific
// cookie name. A random persisted namespace distinguishes two dashboards even
// when their data directories have the same textual path on different
// machines and both are forwarded into one browser's loopback host.
func loadOrCreateDashboardCookieName(dataDir string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", fmt.Errorf("dashboard cookie namespace: empty data directory")
	}
	path := filepath.Join(dataDir, dashboardCookieNamespaceFile)
	if data, err := os.ReadFile(path); err == nil {
		namespace := strings.TrimSpace(string(data))
		if !validDashboardCookieNamespace(namespace) {
			return "", fmt.Errorf("dashboard cookie namespace: invalid value in %s", path)
		}
		return dashboardCookieNameForNamespace(namespace), nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("dashboard cookie namespace: read %s: %w", path, err)
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("dashboard cookie namespace: generate: %w", err)
	}
	namespace := hex.EncodeToString(random[:])
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("dashboard cookie namespace: create data directory: %w", err)
	}
	tmp, err := os.CreateTemp(dataDir, "dashboard-cookie-*.tmp")
	if err != nil {
		return "", fmt.Errorf("dashboard cookie namespace: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(namespace + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("dashboard cookie namespace: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("dashboard cookie namespace: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("dashboard cookie namespace: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("dashboard cookie namespace: install: %w", err)
	}
	return dashboardCookieNameForNamespace(namespace), nil
}

func validDashboardCookieNamespace(namespace string) bool {
	if len(namespace) != 32 {
		return false
	}
	_, err := hex.DecodeString(namespace)
	return err == nil
}
