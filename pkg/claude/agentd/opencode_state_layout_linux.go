//go:build linux

package agentd

import "github.com/tofutools/tclaude/pkg/claude/session"

func adaptOpenCodeStateLayoutForPlatform(*openCodeStateLayout) error {
	return nil
}

func prepareOpenCodeReadOnlyConfigForPlatform(*session.TclaudeLayerLaunchSpec) error {
	return nil
}
