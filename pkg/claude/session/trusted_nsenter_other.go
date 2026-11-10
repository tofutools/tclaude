//go:build !linux

package session

import "errors"

// TrustedNsenterPath is Linux-only: nsenter and network namespaces do not
// exist elsewhere.
func TrustedNsenterPath() (string, error) {
	return "", errors.New("nsenter is Linux-only")
}
