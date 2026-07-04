package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent task-force ls` + `task-force status` (JOH-346) — the read-back
// twins of `deploy`. Deploy records a mission + source template on the group row
// and the dashboard renders a rich force block (mission, phase + transitions,
// per-role liveness, wave progress, rhythm cron jobs), but no CLI verb surfaced
// any of it. These two compose the existing group reads the dashboard uses — the
// groups list (deploy provenance), the process GET, the waves GET, the group
// context GET (per-member liveness), and the cron list (filtered to the group's
// rhythms) — into a CLI-first view. Both are read-only.
//
// "Is a deployed force" is a group row with a non-empty source_template (a plain
// hand-built group carries none, and `groups ls` already covers it). `ls` filters
// on it; `status` refuses a plain group with a pointer to `groups ls`.

// ---- shared wire mirrors ----

// taskForceGroupView is the subset of the /v1/groups summary the read-back verbs
// need: identity, member counts, and the deploy provenance (mission / source
// template) that tells a force apart from a plain group.
type taskForceGroupView struct {
	Name           string `json:"name"`
	Descr          string `json:"descr,omitempty"`
	Members        int    `json:"members"`
	Online         int    `json:"online"`
	Mission        string `json:"mission,omitempty"`
	SourceTemplate string `json:"source_template,omitempty"`
}

// waveStatusView mirrors the daemon's staged-spawn status (agentd/waves.go). The
// waves GET 404s when a deploy has no pending choreography (single-wave, or the
// deploy completed) — the caller treats that as "no pending waves", not an error.
type waveStatusView struct {
	CurrentWave   int    `json:"current_wave"`
	TotalWaves    int    `json:"total_waves"`
	PendingWaves  int    `json:"pending_waves"`
	PendingAgents int    `json:"pending_agents"`
	DeadlineAt    string `json:"deadline_at,omitempty"`
}

// forceMemberView mirrors one entry of the group context GET (agentd
// groupContextEntry): the fields the per-role liveness rollup renders. Status is
// the settled hook status the dashboard classifies on, so the CLI rollup and the
// dashboard force block never disagree about who is stalling.
type forceMemberView struct {
	AgentID     string  `json:"agent_id,omitempty"`
	ConvID      string  `json:"conv_id"`
	Title       string  `json:"title"`
	Role        string  `json:"role,omitempty"`
	Online      bool    `json:"online"`
	Status      string  `json:"status,omitempty"`
	HasSnapshot bool    `json:"has_snapshot"`
	ContextPct  float64 `json:"context_pct"`
}

// forceMemberLiveness mirrors the dashboard force block's classifier
// (dashboard/js/render.js forceMemberLiveness): an offline member is 'dead'; an
// online member is 'idle' only when its recorded status is literally idle, and
// 'working' for anything else in flight. Kept byte-for-byte equivalent so the CLI
// and dashboard never disagree about "stalling".
func forceMemberLiveness(online bool, status string) string {
	if !online {
		return "dead"
	}
	if status == "idle" {
		return "idle"
	}
	return "working"
}

// forceLivenessGlyph is the single-char status mark the rollup renders per member
// (matches render.js forceMemberPill: ● working, ○ idle, ✕ dead).
func forceLivenessGlyph(live string) string {
	switch live {
	case "working":
		return "●"
	case "idle":
		return "○"
	default:
		return "✕"
	}
}

// fetchForceProcess reads a group's advisory process state, returning (nil, nil)
// when the group has no process (the process GET 404s — a deploy from a
// process-less template).
func fetchForceProcess(group string) (*processStateView, error) {
	var st processStateView
	err := DaemonRequest(http.MethodGet, "/v1/groups/"+url.PathEscape(group)+"/process", nil, &st, DaemonOpts{})
	if err != nil {
		if isDaemonStatus(err, http.StatusNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &st, nil
}

// fetchForceWaves reads a group's pending staged-spawn status, returning
// (nil, nil) when there is no pending choreography (the waves GET 404s).
func fetchForceWaves(group string) (*waveStatusView, error) {
	var wv waveStatusView
	err := DaemonRequest(http.MethodGet, "/v1/groups/"+url.PathEscape(group)+"/waves", nil, &wv, DaemonOpts{})
	if err != nil {
		if isDaemonStatus(err, http.StatusNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &wv, nil
}

// isDaemonStatus reports whether err is a DaemonError carrying the given HTTP
// status — used to treat an endpoint's documented 404 (no process / no waves) as
// a clean "absent" rather than a hard failure.
func isDaemonStatus(err error, status int) bool {
	de, ok := err.(*DaemonError)
	return ok && de.Status == status
}

// forceGroups fetches the groups list and returns only the deployed forces (a
// non-empty source template), sorted by name.
func forceGroups() ([]taskForceGroupView, error) {
	var groups []taskForceGroupView
	if err := DaemonGet("/v1/groups", &groups); err != nil {
		return nil, err
	}
	forces := make([]taskForceGroupView, 0, len(groups))
	for _, g := range groups {
		if strings.TrimSpace(g.SourceTemplate) != "" {
			forces = append(forces, g)
		}
	}
	sort.Slice(forces, func(i, j int) bool { return forces[i].Name < forces[j].Name })
	return forces, nil
}

// phaseChip renders a process state's current phase for a one-line column /
// header, or "" when there is no process.
func phaseChip(st *processStateView) string {
	if st == nil {
		return ""
	}
	if st.PhaseIndex >= 0 {
		return fmt.Sprintf("%d/%d %s", st.PhaseIndex+1, st.PhaseCount, st.CurrentPhase)
	}
	return st.CurrentPhase
}

// ---- ls ----

type taskForceLsParams struct {
	JSON bool `long:"json" help:"Output the composed force list as JSON."`
}

func taskForceLsCmd() *cobra.Command {
	return boa.CmdT[taskForceLsParams]{
		Use:   "ls",
		Short: "List deployed task forces (groups with a source template)",
		Long: "Lists the deployed task forces — the groups a `deploy` created, tagged with a source template. For " +
			"each: its mission (truncated), source template, current process phase (if any), pending " +
			"staged-spawn waves, and live/total members. A plain hand-built group (no source template) is not a force " +
			"and is not listed — `tclaude agent groups ls` covers those. Composes the same group reads the dashboard " +
			"force block uses. Read-only.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskForceLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskForceLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// taskForceLsEntry is one composed row of `task-force ls` — the group provenance
// joined with its phase + pending-wave read. Also the `--json` element.
type taskForceLsEntry struct {
	Group          string `json:"group"`
	Mission        string `json:"mission,omitempty"`
	SourceTemplate string `json:"source_template,omitempty"`
	CurrentPhase   string `json:"current_phase,omitempty"`
	// PhaseIndex is the 0-based current phase, or -1 when the force has no
	// process. Serialized unconditionally (no omitempty) so a --json consumer
	// reads a real phase 0 the same way it reads any other, rather than losing
	// the field to omitempty and having to infer it from current_phase.
	PhaseIndex   int `json:"phase_index"`
	PhaseCount   int `json:"phase_count"`
	PendingWaves int `json:"pending_waves"`
	TotalWaves   int `json:"total_waves,omitempty"`
	Online       int `json:"online"`
	Members      int `json:"members"`
}

func runTaskForceLs(p *taskForceLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	forces, err := forceGroups()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}

	entries := make([]taskForceLsEntry, 0, len(forces))
	for _, g := range forces {
		e := taskForceLsEntry{
			Group:          g.Name,
			Mission:        g.Mission,
			SourceTemplate: g.SourceTemplate,
			PhaseIndex:     -1,
			Online:         g.Online,
			Members:        g.Members,
		}
		// Phase + pending waves compose the two per-group reads the dashboard
		// force block uses. Each is optional (a deploy may have no process /
		// no pending waves) — an absent read leaves the column blank.
		st, err := fetchForceProcess(g.Name)
		if err != nil {
			fmt.Fprintf(stderr, "Error: reading process for %q: %v\n", g.Name, err)
			return MapDaemonErrorToRC(err)
		}
		if st != nil {
			e.CurrentPhase = st.CurrentPhase
			e.PhaseIndex = st.PhaseIndex
			e.PhaseCount = st.PhaseCount
		}
		wv, err := fetchForceWaves(g.Name)
		if err != nil {
			fmt.Fprintf(stderr, "Error: reading waves for %q: %v\n", g.Name, err)
			return MapDaemonErrorToRC(err)
		}
		if wv != nil {
			e.PendingWaves = wv.PendingWaves
			e.TotalWaves = wv.TotalWaves
		}
		entries = append(entries, e)
	}

	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			return rcIOFailure
		}
		return rcOK
	}

	if len(entries) == 0 {
		fmt.Fprintln(stdout, "(no deployed task forces — deploy one with `tclaude agent task-force deploy`)")
		return rcOK
	}

	tbl := table.New(
		table.Column{Header: "GROUP", MinWidth: 8, Weight: 0.7, Truncate: true},
		table.Column{Header: "MISSION", MinWidth: 12, Weight: 1.6, Truncate: true},
		table.Column{Header: "TEMPLATE", MinWidth: 8, Weight: 0.7, Truncate: true},
		table.Column{Header: "PHASE", MinWidth: 6, Weight: 0.7, Truncate: true},
		table.Column{Header: "WAVES", Width: 6},
		table.Column{Header: "LIVE/TOTAL", Width: 10},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, e := range entries {
		phase := "—"
		if e.CurrentPhase != "" {
			if e.PhaseIndex >= 0 {
				phase = fmt.Sprintf("%d/%d %s", e.PhaseIndex+1, e.PhaseCount, e.CurrentPhase)
			} else {
				phase = e.CurrentPhase
			}
		}
		waves := "—"
		if e.PendingWaves > 0 {
			waves = fmt.Sprintf("%d", e.PendingWaves)
		}
		liveTotal := fmt.Sprintf("%d/%d", e.Online, e.Members)
		if e.Online == 0 {
			liveTotal += " ⏸" // dormant: no live members
		}
		tbl.AddRow(table.Row{Cells: []string{
			e.Group, oneLine(e.Mission), e.SourceTemplate, phase, waves, liveTotal,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	fmt.Fprintln(stdout, "⏸ = dormant (no live members). `task-force status <group>` for the full block.")
	return rcOK
}

// ---- status ----

type taskForceStatusParams struct {
	Name string `pos:"true" optional:"true" help:"The task force (group) to inspect. Inferred when you are in exactly one group."`
	JSON bool   `long:"json" help:"Output the composed force status as JSON."`
}

func taskForceStatusCmd() *cobra.Command {
	return boa.CmdT[taskForceStatusParams]{
		Use:   "status [group]",
		Short: "Show a deployed force's mission, phase, liveness, waves and rhythms",
		Long: "The CLI twin of the dashboard force block. Shows a deployed task force's mission + provenance (source " +
			"template), its process phase / phase map / recent transitions, a per-role liveness rollup (working / idle / " +
			"offline, with context %% where the member snapshot carries it), any pending staged-spawn waves, and the " +
			"group's rhythm cron jobs. A stood-down force (retired roster, swept rhythms) still shows its mission + " +
			"history and reads as dormant. Refuses a plain group (no source template) — use `tclaude agent groups ls`. " +
			"Composes the group reads the dashboard uses; read-only.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskForceStatusParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *taskForceStatusParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskForceStatus(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// taskForceStatusView is the composed `task-force status` payload — the group
// provenance joined with the process, waves, per-member liveness and rhythm reads.
// Also the `--json` shape. MembersAccessible is false when the caller lacks the
// context-read permission (the liveness rollup + context %% are then omitted; the
// human operator and group owners always pass).
type taskForceStatusView struct {
	Group             string            `json:"group"`
	Descr             string            `json:"descr,omitempty"`
	Mission           string            `json:"mission,omitempty"`
	SourceTemplate    string            `json:"source_template,omitempty"`
	Dormant           bool              `json:"dormant"`
	Online            int               `json:"online"`
	Total             int               `json:"total"`
	Process           *processStateView `json:"process,omitempty"`
	Waves             *waveStatusView   `json:"waves,omitempty"`
	MembersAccessible bool              `json:"members_accessible"`
	Members           []forceMemberView `json:"members,omitempty"`
	Rhythms           []cronJobJSON     `json:"rhythms"`
}

func runTaskForceStatus(p *taskForceStatusParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	group, rc := resolveCallerGroup(p.Name, stderr)
	if rc != rcOK {
		return rc
	}

	// Group meta (provenance + member counts) comes from the groups list — the
	// same source `ls` filters on. A missing group is a clear error; a group
	// with no source template is a plain group, refused with a pointer to
	// `groups ls` (consistent with `ls`'s force filter).
	var groups []taskForceGroupView
	if err := DaemonGet("/v1/groups", &groups); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	var meta *taskForceGroupView
	for i := range groups {
		if groups[i].Name == group {
			meta = &groups[i]
			break
		}
	}
	if meta == nil {
		fmt.Fprintf(stderr, "Error: no group named %q\n", group)
		return rcNotFound
	}
	if strings.TrimSpace(meta.SourceTemplate) == "" {
		fmt.Fprintf(stderr, "Error: group %q is not a deployed task force (no source template). "+
			"Use `tclaude agent groups ls` for plain groups.\n", group)
		return rcInvalidArg
	}

	view := taskForceStatusView{
		Group:          meta.Name,
		Descr:          meta.Descr,
		Mission:        meta.Mission,
		SourceTemplate: meta.SourceTemplate,
		Online:         meta.Online,
		Total:          meta.Members,
		Dormant:        meta.Online == 0,
		Rhythms:        []cronJobJSON{},
	}

	// Process + waves — the two optional per-group reads (404 → absent).
	st, err := fetchForceProcess(group)
	if err != nil {
		fmt.Fprintf(stderr, "Error: reading process: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	view.Process = st
	wv, err := fetchForceWaves(group)
	if err != nil {
		fmt.Fprintf(stderr, "Error: reading waves: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	view.Waves = wv

	// Per-member liveness rides the group context read — the gated surface that
	// carries each member's settled status + context %%. The human operator and
	// group owners always pass; a plain agent without context access degrades
	// gracefully (MembersAccessible=false), still seeing mission / phase / waves
	// / rhythms.
	var members []forceMemberView
	cerr := DaemonRequest(http.MethodGet, "/v1/groups/"+url.PathEscape(group)+"/context", nil, &members, DaemonOpts{})
	switch {
	case cerr == nil:
		view.MembersAccessible = true
		view.Members = members
	case isDaemonStatus(cerr, http.StatusForbidden):
		view.MembersAccessible = false
	default:
		fmt.Fprintf(stderr, "Error: reading members: %v\n", cerr)
		return MapDaemonErrorToRC(cerr)
	}

	// Rhythms — the cron jobs targeting this group. The cron list is
	// caller-scoped (the human sees all; an owner sees its group's jobs), so we
	// filter client-side to this group's group-target jobs rather than adding a
	// server filter.
	var cronResp struct {
		Jobs []cronJobJSON `json:"jobs"`
	}
	if err := DaemonRequest(http.MethodGet, "/v1/cron", nil, &cronResp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: reading rhythms: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	for _, j := range cronResp.Jobs {
		if j.TargetKind == "group" && j.GroupName == group {
			view.Rhythms = append(view.Rhythms, j)
		}
	}
	sort.SliceStable(view.Rhythms, func(i, k int) bool { return view.Rhythms[i].ID < view.Rhythms[k].ID })

	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(view); err != nil {
			return rcIOFailure
		}
		return rcOK
	}

	printTaskForceStatus(stdout, &view)
	return rcOK
}

// printTaskForceStatus renders the human-first force block: the mission +
// provenance header, the phase map + transitions, the per-role liveness rollup,
// the pending waves, and the rhythm cron jobs.
func printTaskForceStatus(stdout io.Writer, v *taskForceStatusView) {
	header := fmt.Sprintf("Task force %q", v.Group)
	if v.Dormant {
		header += "  (dormant — no live members)"
	}
	fmt.Fprintln(stdout, header)
	if m := oneLine(v.Mission); m != "" {
		fmt.Fprintf(stdout, "  Mission:   %s\n", m)
	}
	fmt.Fprintf(stdout, "  Template:  %s\n", v.SourceTemplate)
	fmt.Fprintf(stdout, "  Members:   %d live / %d total\n", v.Online, v.Total)

	// Phase map + transitions (reuses the process show renderer's conventions).
	if v.Process != nil {
		fmt.Fprintf(stdout, "  Phase:     %s\n", orDash(phaseChip(v.Process)))
		for i, ph := range v.Process.Phases {
			marker := "  "
			if ph.Current {
				marker = "▸ "
			}
			roles := "(any)"
			if len(ph.Roles) > 0 {
				roles = strings.Join(ph.Roles, ", ")
			}
			fmt.Fprintf(stdout, "    %s%d. %s  [roles: %s]\n", marker, i+1, ph.Name, roles)
		}
		if len(v.Process.Transitions) > 0 {
			fmt.Fprintln(stdout, "    transitions:")
			for _, tr := range v.Process.Transitions {
				from := tr.From
				if from == "" {
					from = "(start)"
				}
				line := fmt.Sprintf("      %s → %s", from, tr.To)
				if tr.Actor != "" {
					line += "  by " + tr.Actor
				}
				if tr.At != "" {
					line += "  " + tr.At
				}
				fmt.Fprintln(stdout, line)
			}
		}
	}

	// Per-role liveness rollup — mirrors the dashboard force block's grouping.
	if v.MembersAccessible {
		printForceRolesRollup(stdout, v.Members)
	} else {
		fmt.Fprintln(stdout, "  Roles:     (liveness detail needs context access — not shown)")
	}

	// Pending waves.
	if v.Waves != nil && v.Waves.PendingWaves > 0 {
		line := fmt.Sprintf("  Waves:     %d pending (of %d)", v.Waves.PendingWaves, v.Waves.TotalWaves)
		if v.Waves.PendingAgents > 0 {
			line += fmt.Sprintf(", %d agent%s to come", v.Waves.PendingAgents, plural(v.Waves.PendingAgents))
		}
		if v.Waves.DeadlineAt != "" {
			line += "  next gate " + v.Waves.DeadlineAt
		}
		fmt.Fprintln(stdout, line)
	}

	// Rhythms.
	if len(v.Rhythms) == 0 {
		fmt.Fprintln(stdout, "  Rhythms:   (none)")
	} else {
		fmt.Fprintln(stdout, "  Rhythms:")
		for _, j := range v.Rhythms {
			name := j.Name
			if name == "" {
				name = truncate(j.Body, 32)
			}
			role := ""
			if j.TargetRole != "" && j.TargetRole != "all" {
				role = "  (role: " + j.TargetRole + ")"
			}
			fmt.Fprintf(stdout, "    %-20s  %-10s  %s%s\n",
				name, cronScheduleLabel(j), rhythmEnabledLabel(j), role)
		}
	}
}

// printForceRolesRollup groups members by role (first-seen order) and prints a
// per-role line of liveness pills — the "who is working / idle / dead" glance,
// matching the dashboard force block's forceRolesRollup.
func printForceRolesRollup(stdout io.Writer, members []forceMemberView) {
	order := []string{}
	byRole := map[string][]forceMemberView{}
	for _, m := range members {
		role := m.Role
		if role == "" {
			role = "(no role)"
		}
		if _, ok := byRole[role]; !ok {
			order = append(order, role)
		}
		byRole[role] = append(byRole[role], m)
	}
	if len(order) == 0 {
		fmt.Fprintln(stdout, "  Roles:     (no members)")
		return
	}
	fmt.Fprintln(stdout, "  Roles:")
	for _, role := range order {
		pills := make([]string, 0, len(byRole[role]))
		for _, m := range byRole[role] {
			live := forceMemberLiveness(m.Online, m.Status)
			name := m.Title
			if name == "" {
				name = short(m.ConvID)
			}
			pill := forceLivenessGlyph(live) + " " + name
			if m.HasSnapshot && m.ContextPct > 0 {
				pill += fmt.Sprintf(" (%.0f%%)", m.ContextPct)
			}
			pills = append(pills, pill)
		}
		fmt.Fprintf(stdout, "    %-10s %s\n", role+":", strings.Join(pills, "   "))
	}
}

// rhythmEnabledLabel renders a rhythm cron job's enabled state, distinguishing a
// tclaude-auto-paused job (a retire that emptied the group) from a human-paused
// one so the operator can tell which is which (JOH-346 / JOH-345).
func rhythmEnabledLabel(j cronJobJSON) string {
	if j.Enabled {
		return "enabled"
	}
	if j.DisabledReason == "group-retired" {
		return "disabled (auto: group-retired)"
	}
	return "disabled"
}

// orDash returns s, or "—" when s is empty — for a human-first column that should
// never render blank.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
