package agent

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// renameCmd is `tclaude agent rename "<title>"` — invokes the daemon's
// /v1/whoami/rename endpoint, which (when the caller has the
// `self.rename` permission) injects `/rename <title>` into the caller's
// own CC pane via tmux send-keys.
//
// Permissions live in ~/.tclaude/config.json under `agent.default_permissions`
// or `agent.permission_overrides[<conv>]`. The human controls them.
func renameCmd() *cobra.Command {
	return boa.CmdT[renameParams]{
		Use:         "rename",
		Short:       "Rename the current conversation (requires self.rename permission)",
		Long:        "Asks tclaude agentd to inject `/rename <title>` into the caller's own CC pane. Requires the `self.rename` permission, granted by the human via agent.default_permissions or agent.permission_overrides in ~/.tclaude/config.json.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *renameParams, _ *cobra.Command, _ []string) {
			os.Exit(runRename(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type renameParams struct {
	Title string `pos:"true" help:"New conversation title"`
}

func runRename(p *renameParams, stdout, stderr io.Writer) int {
	title := strings.TrimSpace(p.Title)
	if title == "" {
		fmt.Fprintf(stderr, "Error: title is required\n")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ConvID string `json:"conv_id"`
		Title  string `json:"title"`
		Note   string `json:"note,omitempty"`
	}
	if err := DaemonPost("/v1/whoami/rename", map[string]string{"title": title}, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Renamed %s to %q\n", short(resp.ConvID), resp.Title)
	if resp.Note != "" {
		fmt.Fprintf(stdout, "Note: %s\n", resp.Note)
	}
	return rcOK
}
