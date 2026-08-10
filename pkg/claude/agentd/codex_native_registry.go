package agentd

import (
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// A seam because flow tests replace the host boundary. Production always uses
// the ownership/symlink/mode validator; focused tests can install a disposable
// root without touching the operator's /etc or guarded state.
var codexNativeRegistryReadiness = session.ValidateCodexNativeRegistrySetup

func SetCodexNativeRegistryReadinessForTest(fn func() error) func() {
	previous := codexNativeRegistryReadiness
	if fn == nil {
		codexNativeRegistryReadiness = session.ValidateCodexNativeRegistrySetup
	} else {
		codexNativeRegistryReadiness = fn
	}
	return func() { codexNativeRegistryReadiness = previous }
}

func codexNativeRegistryErrorCode(err error) string {
	var setupErr *session.CodexNativeRegistryError
	if errors.As(err, &setupErr) {
		return setupErr.Code
	}
	return "codex_native_registry_not_ready"
}
