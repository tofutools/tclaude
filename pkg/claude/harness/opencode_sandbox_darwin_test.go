//go:build darwin

package harness

import (
	"strings"
	"testing"
)

func TestOpenCodeTclaudeLayerWarnsAboutDarwinConfigBasePrivacy(t *testing.T) {
	warnings := openCodeSandboxWarnings(OpenCodeSandboxTclaudeLayer)
	if len(warnings) != 1 {
		t.Fatalf("Darwin tclaude-layer warnings = %v, want config-base privacy disclosure", warnings)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{
		"mutable XDG privacy covers OpenCode data/cache/state only",
		"config base is not redirected",
		"real host config base",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Darwin tclaude-layer warnings %q missing %q", joined, want)
		}
	}
}
