//go:build linux || darwin

package host

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// SandboxRuntimeRules declares the platform's read-only executable/library
// resources. Callers still bind these through the protected-root inspector;
// this declaration cannot exempt a backend-private root from that check.
// Provider configuration and user files require their own explicit resources.
func SandboxRuntimeRules() ([]model.SandboxFilesystemRule, error) {
	paths := []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"}
	if runtime.GOOS == "darwin" {
		// Keep the system library regions explicit: granting all of /System
		// would also include its Data volume. Modern dyld caches live in the
		// Cryptex regions, independently of /System/Library.
		paths = []string{"/System/Library", "/usr", "/bin", "/sbin", "/Library/Apple",
			"/System/Cryptexes/OS", "/System/Cryptexes/App",
			"/System/Volumes/Preboot/Cryptexes/OS", "/System/Volumes/Preboot/Cryptexes/App/System"}
	}
	var rules []model.SandboxFilesystemRule
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("sandbox system directory unavailable: %s", path)
		}
		rules = append(rules, model.SandboxFilesystemRule{HostPath: path, GuestPath: path, Access: model.SandboxFilesystemRead, ExpectedKind: "directory"})
	}
	if runtime.GOOS == "darwin" {
		// Apple's curl initializes LibreSSL even for HTTP over a Unix socket.
		// Its system configuration is a runtime dependency, not a grant for
		// the surrounding /private/etc tree or provider/user configuration.
		path := "/private/etc/ssl/openssl.cnf"
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("sandbox system TLS configuration unavailable: %s", path)
		}
		rules = append(rules, model.SandboxFilesystemRule{HostPath: path, GuestPath: path, Access: model.SandboxFilesystemRead, ExpectedKind: "file"})
	}
	return rules, nil
}
