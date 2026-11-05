package session

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// launchShellEnvironment is the environment the pane's launch shell starts
// with: the sandbox profile's own, plus, for a constructed-root tclaude-layer
// launch, the agentd socket this launch selected.
func launchShellEnvironment(
	effectiveSandbox *sandboxpolicy.Snapshot,
	outerLayer bool,
	root sandboxpolicy.RootPosture,
	additionalEnv map[string]string,
) map[string]string {
	shellEnvironment := sandboxSnapshotEnvironment(effectiveSandbox)
	if outerLayer && root == sandboxpolicy.RootConstructed {
		if selected := additionalEnv[agentipc.SocketEnv]; selected != "" {
			if shellEnvironment == nil {
				shellEnvironment = map[string]string{}
			}
			shellEnvironment[agentipc.SocketEnv] = selected
		}
	}
	return shellEnvironment
}

// finishHarnessSpawnSpec fills in the parts of the spawn spec that depend on
// the harness: pi's natively registered instructions and, behind tclaude's
// wall, its walled agent directory; and a harness's live-status extension.
// It returns the extension's directory when tclaude's sandbox must mount it
// for the pane.
func finishHarnessSpawnSpec(
	h *harness.Harness,
	spec *harness.SpawnSpec,
	params *NewParams,
	outerLayer bool,
	root sandboxpolicy.RootPosture,
	approvalPolicy string,
) ([]string, error) {
	var readPaths []string
	if h.Name == harness.PiName {
		// Verified above; pi registers its skills and rules natively.
		spec.ManagedInstructionsRoot = params.ManagedInstructionsRoot
	}
	if outerLayer && h.Name == harness.PiName {
		// A walled pi runs against tclaude's walled agent directory, never
		// the operator's (pi_walled_agent.go), and one of its own: the
		// directory is keyed by the conversation it launches.
		conversation := spec.ResumeID
		if conversation == "" {
			conversation = spec.SessionID
		}
		walled, err := harness.PreparePiWalledAgentDir(conversation)
		if err != nil {
			return nil, err
		}
		spec.HarnessStateDir = walled
	}
	if h.StatusExtension != nil {
		// The extension is the harness's only status channel. Failing to
		// write it costs live status, not the launch: say so plainly.
		path, err := h.StatusExtension()
		if err != nil && h.Name == harness.PiName && approvalPolicy == harness.PiApprovalAsk {
			// ask's gate is the permission extension written beside it.
			return nil, fmt.Errorf("pi's ask posture needs tclaude's permission extension: %w", err)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %s live status is unavailable for this session: %v\n", h.DisplayName, err)
			slog.Warn("status extension unavailable", "harness", h.Name, "error", err, "module", "session")
		} else {
			spec.StatusExtensionPath = path
			if outerLayer {
				// The extension runs inside the pane, so tclaude's sandbox
				// mounts it read-only (its directory holds only this
				// version's file), and it reaches tclaude where the sandbox
				// puts it.
				readPaths = append(readPaths, filepath.Dir(path))
				if root == sandboxpolicy.RootConstructed {
					spec.TclaudeExecutable = tclaudeLayerConstructedRootTclaudePath
				}
			}
		}
	}
	return readPaths, nil
}
