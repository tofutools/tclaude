//go:build !linux

package session

import (
	"fmt"
)

func resolveBwrapBinary() (string, error) {
	return "", fmt.Errorf("tclaude-layer requires Linux and bubblewrap; this platform is not supported")
}
