// Package workgraphcli implements `tclaude workgraph` — a thin client over the
// tclaude agentd Unix socket that lets agents (and humans) introspect and drive
// workgraphs from the terminal, mirroring how `tclaude agent` works.
//
// Template discovery (templates/show) is done client-side against the caller's
// own project + user + example sources via the workgraph package — templates are
// plain files on disk, not DB rows. Everything that touches a running instance
// (ls instances, status, events, where, new, node, cancel, rm) goes through the
// daemon's peer-cred /v1/workgraphs* surface, so the daemon stays the single
// owner of the SQLite store.
//
// See the workgraph-CLI ticket on Linear (JOH-13).
package workgraphcli

import (
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/workgraph"
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

// Cmd returns the `tclaude workgraph` cobra command.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "workgraph",
		Short: "Introspect and drive workgraphs",
		Long: "Introspect and drive tclaude workgraphs from the terminal.\n\n" +
			"Template discovery (templates, show) reads the project / user / example\n" +
			"sources directly — templates are plain files on disk, so these need no\n" +
			"daemon. Instance commands (ls, status, events, where, new, node, cancel,\n" +
			"rm) talk to the running tclaude agentd daemon and land alongside its\n" +
			"/v1/workgraphs API.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			lsCmd(),
			templatesCmd(),
			showCmd(),
			installCmd(),
			statusCmd(),
			eventsCmd(),
			whereCmd(),
			newCmd(),
			nodeCmd(),
			spawnCmd(),
			driveCmd(),
			cancelCmd(),
			rmCmd(),
		},
	}.ToCobra()
}

// projectDirs yields the project-source template directories searched for
// client-side discovery: the workgraphs dir of the caller's current working
// directory. Mirrors agentd's defaultWorkgraphProjectDirs, but anchored on the
// CLI's cwd (where the operator runs `tclaude workgraph`) rather than the
// daemon's, so "the templates I can see here" matches the shell's location.
func projectDirs() []string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return nil
	}
	pd := workgraph.ProjectDir(wd)
	if pd == "" {
		return nil
	}
	return []string{pd}
}
