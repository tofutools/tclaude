//go:build linux

package agentd

import "github.com/tofutools/tclaude/pkg/claude/session"

func adaptOpenCodeStateLayoutForPlatform(*openCodeStateLayout) error {
	return nil
}

// prepareOpenCodeReadOnlyConfigForPlatform delegates to the shared
// implementation; see prepareOpenCodeReadOnlyConfig.
//
// This was a no-op until TCL-892: a private-state launch binds the config app
// directory read-only, so OpenCode's own bootstrap write during the FIRST
// session creation failed with EROFS and the server answered an opaque
// HTTP 500.
func prepareOpenCodeReadOnlyConfigForPlatform(
	spec *session.TclaudeLayerLaunchSpec,
) error {
	return prepareOpenCodeReadOnlyConfig(spec, "Linux")
}
