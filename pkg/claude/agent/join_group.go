package agent

import (
	"fmt"
	"net/http"
	"os"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

func init() {
	session.JoinGroupHandler = runJoinGroup
}

// runJoinGroup implements `tclaude --join-group <group>` (and the
// equivalent flag on `tclaude session new`).
//
// Spawns a fresh CC session via the daemon's existing groups-spawn
// orchestration (same code path `tclaude agent spawn` uses), then —
// unless `-d` was given — attaches to the new tmux session in the
// foreground so the human lands directly in the new agent's pane.
//
// Permission: same `groups.spawn` slug as `agent spawn` (default
// human-only). Humans bypass the permission check entirely.
func runJoinGroup(params *session.NewParams) error {
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
	body := map[string]any{
		"alias":           params.Alias,
		"role":            params.Role,
		"descr":           params.Descr,
		"cwd":             cwd,
		"timeout_seconds": 30,
	}
	var resp struct {
		Group       string `json:"group"`
		ConvID      string `json:"conv_id"`
		Label       string `json:"label"`
		TmuxSession string `json:"tmux_session"`
		AttachCmd   string `json:"attach_cmd"`
	}
	path := "/v1/groups/" + params.JoinGroup + "/spawn"
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{}); err != nil {
		return fmt.Errorf("spawn into group %q: %w", params.JoinGroup, err)
	}

	fmt.Printf("Spawned %s in group %q\n", short(resp.ConvID), resp.Group)
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
