//go:build !linux

package session

import "github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"

func PrepareNetworkSyncLaunch(_ *TclaudeLayerLaunchSpec, _ *sandboxpolicy.Snapshot, _ string) error {
	return nil
}
