package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent task-force` — deploy a task force against a mission
// (JOH-245).
//
// The headline use case made first-class: "deploy task force X to address
// topic / problem / epic Y." `deploy` is a thin, mission-framed verb over
// the same daemon path `templates instantiate` uses — it creates a fresh
// group and spawns the template's whole team, folding the mission into the
// group context under "## Mission" (instantiate's "## Task" analogue) and
// recording the mission + source template on the group so the dashboard can
// show the group as a deployed force. `templates instantiate` keeps working
// unchanged; deploy adds the mission framing, group-name derivation, and a
// deploy-level `--worktree`.

func taskForceCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "task-force",
		Short:       "Deploy a task force against a mission",
		Long: "Deploy a whole agent team against a topic, problem or epic. `deploy` wraps a group template: it " +
			"creates a fresh group, spawns the template's team, and folds the --mission into the group context " +
			"every agent sees. The mission-framed twin of `templates instantiate`.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			taskForceDeployCmd(),
			taskForceLsCmd(),
			taskForceStatusCmd(),
			taskForceStandDownCmd(),
		},
	}.ToCobra()
}

// ---- deploy ----

type taskForceDeployParams struct {
	Name         string `pos:"true" help:"Template to deploy (from 'tclaude agent templates ls'). Its roster becomes the task force."`
	Mission      string `long:"mission" short:"m" optional:"true" help:"The topic / problem / epic to deploy against — free text or a Linear epic/issue link. Folded into the group context under '## Mission'. Use --mission-file for long or multi-line text."`
	MissionFile  string `long:"mission-file" optional:"true" help:"Read the mission text from this file ('-' reads stdin). Sidesteps shell quoting; best for long, multi-line missions. Mutually exclusive with --mission."`
	Group        string `long:"group" optional:"true" help:"Name for the new group (also the prefix for every spawned agent). Defaults to a name derived from the mission."`
	Cwd          string `long:"cwd" optional:"true" help:"Working directory the force spawns in (~ expands). Must exist. Empty inherits the daemon's cwd. Ignored when --worktree is set (the worktree becomes the cwd)."`
	Descr        string `long:"descr" optional:"true" help:"One-line description for the new group. Defaults to 'Task force deployed from template <name>'."`
	Worktree     string `long:"worktree" short:"w" optional:"true" help:"Create (or reuse) a git worktree on this branch and land the WHOLE force in it. The worktree is resolved in-process in the repo containing --cwd (or, when --cwd is empty, the current directory)."`
	WorktreeBase string `long:"worktree-base" optional:"true" help:"Base branch for a newly-created --worktree (default: the repo's default branch). Ignored when the --worktree branch already exists."`
}

func taskForceDeployCmd() *cobra.Command {
	return boa.CmdT[taskForceDeployParams]{
		Use:   "deploy <template> --mission <text>",
		Short: "Deploy a task force from a template against a mission",
		Long: "Creates a fresh group and spawns one agent per template spec, framed as a task force deployed " +
			"against --mission. The mission is folded into the group's shared context under '## Mission', so every " +
			"spawned agent's startup briefing carries it. With no --group, a group name is derived from the mission. " +
			"With --worktree, the whole force lands on its own branch in a git worktree. `deploy` is the mission-framed " +
			"form of `templates instantiate` (an alias over the same path) — a Linear-link mission is stored verbatim " +
			"(tclaude pulls no title).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskForceDeployParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeTemplateNames)
			return nil
		},
		RunFunc: func(p *taskForceDeployParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskForceDeploy(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// deployResponse mirrors the daemon's deploy response — the instantiate
// shape plus the deploy framing (mission / deployed).
type deployResponse struct {
	instantiateResponse
	Deployed bool   `json:"deployed"`
	Mission  string `json:"mission"`
}

func runTaskForceDeploy(p *taskForceDeployParams, stdin io.Reader, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a template name is required")
		return rcInvalidArg
	}
	// --worktree-base only makes sense with --worktree (mirrors spawn).
	if strings.TrimSpace(p.Worktree) == "" && strings.TrimSpace(p.WorktreeBase) != "" {
		fmt.Fprintln(stderr, "Error: --worktree-base requires --worktree")
		return rcInvalidArg
	}
	mission, rc := resolveBodyInput(p.Mission, p.MissionFile, "--mission", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(mission) == "" {
		fmt.Fprintln(stderr, "Error: a mission is required (give --mission or --mission-file)")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}

	cwd := strings.TrimSpace(p.Cwd)

	// Worktree handling — the CLI resolves it in-process (the same git
	// operation the dashboard picker performs), then hands the resolved path
	// down as the whole force's cwd. createdWorktree is non-empty only when a
	// fresh worktree was made (vs an existing one reused), so a failed deploy
	// can tear it back down rather than leaking an orphan.
	createdWorktree := ""
	if wt := strings.TrimSpace(p.Worktree); wt != "" {
		worktreeRepo := cwd
		wtPath, createdNew, wtErr := resolveSpawnWorktree(worktreeRepo, wt, p.WorktreeBase)
		if wtErr != nil {
			fmt.Fprintf(stderr, "Error: %v\n", wtErr)
			return rcInvalidArg
		}
		if createdNew {
			createdWorktree = wtPath
		}
		cwd = wtPath
	}

	body := map[string]any{"mission": mission}
	if g := strings.TrimSpace(p.Group); g != "" {
		body["group_name"] = g
	}
	if cwd != "" {
		body["cwd"] = cwd
	}
	if d := strings.TrimSpace(p.Descr); d != "" {
		body["descr"] = d
	}

	var resp deployResponse
	// Deploying a whole team spawns each agent sequentially (each polls for a
	// conv-id), so it can run well past the default 10s budget — same as
	// instantiate. Transparent dir write-proof handling (see writeproof.go).
	deployBody := func(writeProofToken string) any {
		return withWriteProofToken(body, writeProofToken)
	}
	err := DaemonRequestWithWriteProof(http.MethodPost, "/v1/templates/"+url.PathEscape(name)+"/deploy",
		deployBody, &resp, DaemonOpts{Timeout: 5 * time.Minute})
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		// The deploy failed after we created a worktree for it. Remove the
		// now-orphaned worktree so a retry starts clean — except on a 504
		// conv-id-poll timeout, where a spawn subprocess may still be coming
		// up inside it (mirrors spawn's teardown policy). The branch is always
		// kept, so a retry reuses it.
		if createdWorktree != "" {
			if de, ok := err.(*DaemonError); ok && de.Status == http.StatusGatewayTimeout {
				fmt.Fprintf(stderr, "Note: kept the worktree %s — the force may still be coming up.\n", createdWorktree)
			} else if _, rmErr := removeSpawnWorktree(createdWorktree); rmErr != nil {
				fmt.Fprintf(stderr, "Note: could not remove the worktree created for this deploy (%s): %v\n",
					createdWorktree, rmErr)
			} else {
				fmt.Fprintf(stderr, "Note: removed the worktree created for this deploy (%s)\n", createdWorktree)
			}
		}
		return MapDaemonErrorToRC(err)
	}

	fmt.Fprintf(stdout, "Task force %q deployed from template %q against: %s\n",
		resp.Group, resp.Template, oneLine(mission))
	fmt.Fprintf(stdout, "  %d spawned, %d failed\n", resp.Spawned, resp.Failed)
	if cwd != "" {
		if wt := strings.TrimSpace(p.Worktree); wt != "" {
			fmt.Fprintf(stdout, "  Worktree: %s (branch %s)\n", cwd, wt)
		} else {
			fmt.Fprintf(stdout, "  Cwd:      %s\n", cwd)
		}
	}
	for _, a := range resp.Agents {
		if a.Error != "" {
			fmt.Fprintf(stdout, "  ✗ %-24s  %s\n", a.FinalName, a.Error)
			continue
		}
		tags := []string{"conv " + short(a.ConvID)}
		if a.Owner {
			tags = append(tags, "owner")
		}
		if len(a.Granted) > 0 {
			tags = append(tags, "granted: "+strings.Join(a.Granted, ","))
		}
		fmt.Fprintf(stdout, "  ✓ %-24s  %s\n", a.FinalName, strings.Join(tags, "  "))
	}
	if resp.PatternDelivered > 0 {
		fmt.Fprintf(stdout, "  work pattern: %d message%s delivered\n",
			resp.PatternDelivered, plural(resp.PatternDelivered))
	}
	for _, e := range resp.PatternErrors {
		fmt.Fprintf(stdout, "  ⚠ work pattern: %s\n", e)
	}
	printStagedSpawnAndRhythms(stdout, resp.instantiateResponse)
	// A partial (or total) spawn failure is a non-zero exit so scripts notice
	// — the group + any spawned agents still exist for the human to finish or
	// retry by hand. The worktree (if any) is deliberately KEPT: agents are
	// running in it.
	if resp.Failed > 0 {
		fmt.Fprintf(stderr, "Error: %d of %d agent(s) failed to spawn — see above\n",
			resp.Failed, resp.Failed+resp.Spawned)
		return rcIOFailure
	}
	return rcOK
}

// ---- stand-down ----

type taskForceStandDownParams struct {
	Name       string `pos:"true" help:"Group to stand down (a deployed task force, or any group)."`
	NoShutdown bool   `long:"no-shutdown" help:"Leave each retired member's running session alive. By default stand-down also soft-exits the running tmux pane (sends /exit); pass this to keep the processes running."`
	Reason     string `long:"reason" short:"r" optional:"true" help:"Why the force is being stood down (recorded in the audit trail)."`
	AskHuman   string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func taskForceStandDownCmd() *cobra.Command {
	return boa.CmdT[taskForceStandDownParams]{
		Use:   "stand-down <group>",
		Short: "Wind a task force down: retire the roster + sweep its rhythms and pending waves",
		Long: "The mirror of `deploy`. Retires every member of the group (the bulk parallel of `tclaude agent retire`) " +
			"and sweeps the deploy-seeded runtime — the template rhythm cron jobs and any pending startup waves — while " +
			"KEEPING the group row as a dormant record, so the mission, provenance, and process history survive. " +
			"Deliberately NOT a group delete (`tclaude agent groups rm` does that). By default each member's running pane " +
			"is soft-exited (sends /exit); pass --no-shutdown to leave them running. The caller's own conversation is " +
			"always skipped. Standing down a plain group (no template) simply retires its members — there is nothing to " +
			"sweep. Gated: the human always, group owners of the group, otherwise the `groups.retire` permission.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskForceStandDownParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskForceStandDownParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskForceStandDown(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTaskForceStandDown(p *taskForceStandDownParams, stdout, stderr io.Writer) int {
	if strings.TrimSpace(p.Name) == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	// Spell shutdown out explicitly so the CLI default is independent of the
	// server-side default (mirrors `groups retire`).
	q := url.Values{}
	if p.NoShutdown {
		q.Set("shutdown", "0")
	} else {
		q.Set("shutdown", "1")
	}
	if reason := strings.TrimSpace(p.Reason); reason != "" {
		q.Set("reason", reason)
	}
	path := "/v1/groups/" + url.PathEscape(p.Name) + "/stand-down?" + q.Encode()

	var resp struct {
		Group   string `json:"group"`
		Action  string `json:"action"`
		Members []struct {
			AgentID string `json:"agent_id,omitempty"`
			ConvID  string `json:"conv_id"`
			Title   string `json:"title,omitempty"`
			Action  string `json:"action"`
			Detail  string `json:"detail,omitempty"`
		} `json:"members"`
		RhythmsRemoved int      `json:"rhythms_removed"`
		WavesCancelled int      `json:"waves_cancelled"`
		Warnings       []string `json:"warnings,omitempty"`
	}
	opts := DaemonOpts{AskHuman: ask}
	if err := DaemonRequest(http.MethodPost, path, nil, &resp, opts); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}

	fmt.Fprintf(stdout, "Task force %q stood down:\n", resp.Group)
	if len(resp.Members) == 0 {
		fmt.Fprintf(stdout, "  (no members to retire)\n")
	} else {
		tbl := table.New(
			table.Column{Header: "ID", Width: 12},
			table.Column{Header: "NAME", MinWidth: 8, Weight: 0.6, Truncate: true},
			table.Column{Header: "ACTION", MinWidth: 10, Weight: 0.6, Truncate: true},
			table.Column{Header: "DETAIL", MinWidth: 10, Weight: 1.4, Truncate: true},
		)
		tbl.SetTerminalWidth(table.GetTerminalWidth())
		for _, m := range resp.Members {
			name := m.Title
			if name == "" {
				name = "(unnamed)"
			}
			tbl.AddRow(table.Row{Cells: []string{
				shortAgentID(m.AgentID, m.ConvID), name, m.Action, m.Detail,
			}})
		}
		fmt.Fprintln(stdout, tbl.Render())
	}
	fmt.Fprintf(stdout, "Swept: %d rhythm job%s removed, %d pending wave%s cancelled.\n",
		resp.RhythmsRemoved, plural(resp.RhythmsRemoved),
		resp.WavesCancelled, plural(resp.WavesCancelled))
	fmt.Fprintf(stdout, "The group is kept as a dormant record (mission + history preserved). "+
		"Use `tclaude agent groups rm %s` to delete it entirely.\n", resp.Group)
	for _, warn := range resp.Warnings {
		fmt.Fprintf(stdout, "⚠ %s\n", warn)
	}
	return rcOK
}

// oneLine collapses a possibly-multi-line mission to a single trimmed line
// for the one-line "deployed against: …" banner, capping the length so a
// long brief doesn't blow out the terminal.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	return truncate(s, 100)
}
