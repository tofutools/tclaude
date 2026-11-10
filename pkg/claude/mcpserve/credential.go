package mcpserve

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// CredentialEnv is where `tclaude mcp serve` reads the credential from unless
// --credential-file is given. There is deliberately no flag for the value itself:
// other processes can read a command line.
const CredentialEnv = "TCLAUDE_EXTERNAL_STEWARD_CREDENTIAL"

const (
	credentialPrefix  = "tcls_"
	maxCredentialFile = 4096
)

// LoadCredential resolves the credential from a file or the environment, and
// clears the environment copy so nothing this process starts inherits it.
func LoadCredential(file string) (string, error) {
	fromEnv := strings.TrimSpace(os.Getenv(CredentialEnv))
	_ = os.Unsetenv(CredentialEnv)
	var credential string
	switch {
	case file != "" && fromEnv != "":
		return "", fmt.Errorf("both --credential-file and %s are set; use one", CredentialEnv)
	case file != "":
		var err error
		if credential, err = readCredentialFile(file); err != nil {
			return "", err
		}
	case fromEnv != "":
		credential = fromEnv
	default:
		return "", fmt.Errorf("no credential: set %s or pass --credential-file. "+
			"The operator makes one with `tclaude agent steward external add`", CredentialEnv)
	}
	if !strings.HasPrefix(credential, credentialPrefix) {
		return "", errors.New("that is not an external steward credential")
	}
	return credential, nil
}

// readCredentialFile refuses a file anyone else could read or replace. The mode
// comes from the open file, so a swap between the check and the read proves
// nothing. O_NONBLOCK keeps a FIFO from hanging startup before that check.
func readCredentialFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open credential file: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat credential file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("credential file is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("credential file %s is readable by others; run chmod 600 on it", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return "", fmt.Errorf("credential file %s belongs to another user", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCredentialFile+1))
	if err != nil {
		return "", fmt.Errorf("read credential file: %w", err)
	}
	if len(raw) > maxCredentialFile {
		return "", errors.New("credential file is too large")
	}
	return strings.TrimSpace(string(raw)), nil
}
