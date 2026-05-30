// Package workflowcli implements `tclaude workflow` — a thin client over the
// tclaude agentd Unix socket that lets agents (and humans) introspect and drive
// workflows from the terminal, mirroring how `tclaude agent` works.
//
// Template discovery (templates/show) is done client-side against the caller's
// own project + user + example sources via the workflow package — templates are
// plain files on disk, not DB rows. Everything that touches a running instance
// (ls instances, status, events, where, new, node, cancel, rm) goes through the
// daemon's peer-cred /v1/workflows* surface, so the daemon stays the single
// owner of the SQLite store.
//
// See docs/plans/TODO/future/workflows-cli.md for the design.
package workflowcli

import (
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/workflow"
	"github.com/tofutools/tclaude/pkg/common"
)

// Exit codes — same scheme as pkg/claude/agent so agent.MapDaemonErrorToRC
// (which returns these values) lines up with our own direct exits.
const (
	rcOK         = 0
	rcNotFound   = 1
	rcAmbiguous  = 2
	rcInvalidArg = 3
	rcIOFailure  = 4
	rcAuth       = 5
)

// Cmd returns the `tclaude workflow` cobra command.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "workflow",
		Short: "Introspect and drive workflows",
		Long: "Introspect and drive tclaude workflows from the terminal.\n\n" +
			"Template discovery (templates, show) reads the project / user / example\n" +
			"sources directly — templates are plain files on disk, so these need no\n" +
			"daemon. Instance commands (ls, status, events, where, new, node, cancel,\n" +
			"rm) talk to the running tclaude agentd daemon and land alongside its\n" +
			"/v1/workflows API.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			templatesCmd(),
			showCmd(),
		},
	}.ToCobra()
}

// projectDirs yields the project-source template directories searched for
// client-side discovery: the workflows dir of the caller's current working
// directory. Mirrors agentd's defaultWorkflowProjectDirs, but anchored on the
// CLI's cwd (where the operator runs `tclaude workflow`) rather than the
// daemon's, so "the templates I can see here" matches the shell's location.
func projectDirs() []string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return nil
	}
	pd := workflow.ProjectDir(wd)
	if pd == "" {
		return nil
	}
	return []string{pd}
}
