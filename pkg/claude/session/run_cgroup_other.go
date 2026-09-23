//go:build !linux

package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func PrepareRunCgroup(int, sandboxpolicy.ResourceLimits) (string, sandboxpolicy.ResourceLimits, func(), error) {
	return "", sandboxpolicy.ResourceLimits{}, func() {}, fmt.Errorf("run cgroups are Linux only")
}
