package agent

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func init() {
	session.JoinGroupHandler = RunJoinGroup
}

// RunJoinGroup implements `tclaude --join-group <group>` (and the
// equivalent flag on `tclaude session new`).
//
// Spawns a fresh CC session via the daemon's existing groups-spawn
// orchestration (same code path `tclaude agent spawn` uses), then —
// unless `-d` was given — attaches to the new tmux session in the
// foreground so the human lands directly in the new agent's pane.
//
// Permission: same `groups.spawn` slug as `agent spawn` (default
// human-only). Humans bypass the permission check entirely.
func RunJoinGroup(params *session.NewParams) error {
	if params.Resume != "" {
		return fmt.Errorf("--join-group cannot be combined with --resume (spawn always creates a fresh session)")
	}
	if params.Label != "" {
		return fmt.Errorf("--join-group picks its own label; --label is not supported here")
	}
	if rc := RequireDaemonOrExit(os.Stderr); rc != rcOK {
		return fmt.Errorf("daemon required for --join-group")
	}

	cwd := params.Dir
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	// Resolve --harness (default Claude Code) so --effort/--model are
	// validated against the chosen harness's own rules (a Codex join
	// accepts a Codex model, rejects a Claude Code slug). An
	// unknown/not-spawnable harness fails fast here.
	h, err := harness.ResolveSpawnable(strings.TrimSpace(params.Harness))
	if err != nil {
		return err
	}
	// Validate --effort here too: this surface is reachable directly
	// (RunJoinGroup runs both via `session new --effort --join-group`,
	// where runNew already normalised it, and standalone), so a clean
	// client-side error beats a daemon round-trip on a typo.
	effort, err := h.Models.ValidateEffort(params.Effort)
	if err != nil {
		return err
	}
	// Same treatment for --model.
	model, err := h.Models.ValidateModel(params.Model)
	if err != nil {
		return err
	}
	req := SpawnRequest{
		Name:           params.Name,
		Role:           params.Role,
		Descr:          params.Descr,
		Cwd:            cwd,
		Effort:         effort,
		Model:          model,
		Harness:        h.Name,
		TimeoutSeconds: 30,
	}
	proof, proofCwd, cleanupProof, err := prepareSpawnCwdWriteProof(req.Cwd)
	if err != nil {
		return err
	}
	defer cleanupProof()
	if proofCwd != "" {
		req.Cwd = proofCwd
	}
	req.CwdWriteProof = proof
	var resp SpawnResponse
	path := "/v1/groups/" + params.JoinGroup + "/spawn"
	if err := DaemonRequest(http.MethodPost, path, req, &resp, DaemonOpts{}); err != nil {
		return fmt.Errorf("spawn into group %q: %w", params.JoinGroup, err)
	}

	fmt.Printf("Spawned %s in group %q\n", shortAgentID(resp.AgentID, resp.ConvID), resp.Group)
	if resp.Label != "" {
		fmt.Printf("  Label:   %s\n", resp.Label)
	}
	if resp.TmuxSession != "" {
		fmt.Printf("  Tmux:    %s\n", resp.TmuxSession)
	}

	if params.Detached {
		if resp.AttachCmd != "" {
			fmt.Printf("\nAttach with: %s\n", resp.AttachCmd)
		}
		return nil
	}

	fmt.Println("\nAttaching... (Ctrl+B D to detach)")
	return session.AttachToSession(resp.Label, resp.TmuxSession, false)
}
