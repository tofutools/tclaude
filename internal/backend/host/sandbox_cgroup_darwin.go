//go:build darwin

package host

import (
	"context"
	"fmt"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func prepareSandboxCgroup(_ string, limits model.SandboxResources) (*sandboxCgroup, error) {
	if limits == (model.SandboxResources{}) {
		return nil, nil
	}
	return nil, fmt.Errorf("CPU and memory resource limits are Linux only")
}
func (c *sandboxCgroup) verify() error {
	if c == nil {
		return nil
	}
	return fmt.Errorf("resource limits are Linux only")
}
func (c *sandboxCgroup) remove() error                 { return c.verify() }
func (c *sandboxCgroup) kill() error                   { return c.verify() }
func (c *sandboxCgroup) containsCurrentProcess() error { return c.verify() }
func superviseSandboxResources(context.Context, SandboxChildArtifact, *sandboxCgroup) error {
	return fmt.Errorf("resource limits are Linux only")
}

func protectSandboxResourcePaths(*SandboxPathInspector) (*SandboxPathInspector, error) {
	return nil, fmt.Errorf("CPU and memory resource limits are Linux only")
}
