package product

import (
	"context"
	"fmt"

	"github.com/tofutools/tclaude/internal/backend/host"
)

// TrySandboxChild is dispatched before normal product startup in both shipped
// binaries. A native child never opens the backend, reads user configuration or
// authenticates an API request. Its sole input is the prepared host artifact.
func TrySandboxChild(args []string) (bool, error) {
	if len(args) == 0 || args[0] != host.SandboxChildCommand {
		return false, nil
	}
	if len(args) != 3 {
		return true, fmt.Errorf("sandbox child requires its prepared artifact identity")
	}
	return true, host.ExecuteSandboxChild(context.Background(), host.SandboxChildArtifact{Path: args[1], Digest: args[2]})
}
