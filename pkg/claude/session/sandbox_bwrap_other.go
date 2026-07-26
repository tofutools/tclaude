//go:build !linux

package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func resolveBwrapBinary(sandboxpolicy.NetworkPosture) (string, error) {
	return "", fmt.Errorf("tclaude-layer requires Linux and bubblewrap; this platform is not supported")
}
