package product

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
)

// TrySandboxChild is dispatched before normal product startup in both shipped
// binaries. A native child never opens the backend, reads user configuration or
// authenticates an API request. Inputs are the prepared host artifact or a
// provider command already enclosed by that artifact.
func TrySandboxChild(args []string) (bool, error) {
	if len(args) > 0 && args[0] == codex.ForkTerminalCommand {
		if len(args) != 2 || len(args[1]) > 1<<20 {
			return true, fmt.Errorf("invalid Codex fork child command")
		}
		var request codex.ForkTerminalRequest
		if err := json.Unmarshal([]byte(args[1]), &request); err != nil {
			return true, err
		}
		return true, codex.ExecuteForkTerminal(context.Background(), request)
	}
	if len(args) == 0 || args[0] != host.SandboxChildCommand {
		return false, nil
	}
	if len(args) != 3 {
		return true, fmt.Errorf("sandbox child requires its prepared artifact identity")
	}
	return true, host.ExecuteSandboxChild(context.Background(), host.SandboxChildArtifact{Path: args[1], Digest: args[2]})
}
