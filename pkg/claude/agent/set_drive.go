package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent set-drive` is the durable answer to "take this agent off the
// Copilot API drive, and leave it off" (TCL-1082).
//
// Before it, choosing the drive was possible from several surfaces and
// un-choosing it durably from exactly one: a running agentd. That is a poor
// shape for the rollback path of a mechanism the operator has NOT verified —
// a rollback that requires the mechanism owner to be healthy is the rollback
// failing in the case it exists for.
//
// So the send-keys direction survives an unreachable daemon and the api
// direction does not. That asymmetry is the design rather than an omission:
// de-escalating onto the channel every Copilot agent already ran on needs
// nothing but a durable record, while escalating claims a capability only
// agentd can produce (it dials the port; nothing else does).

const (
	setDriveSendKeys = "send-keys"
	setDriveAPI      = "api"
)

// SetDriveParams is exported because the flow tests drive this command through
// the production daemon mux, the same way the spawn path is exercised.
type SetDriveParams struct {
	Agent    string `pos:"true" help:"Agent selector: title, full conv-id, or 8+-char prefix"`
	Drive    string `pos:"true" help:"send-keys | api"`
	JSON     bool   `long:"json" help:"Emit the stable JSON object instead of the human view"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func setDriveCmd() *cobra.Command {
	return boa.CmdT[SetDriveParams]{
		Use:     "set-drive",
		Aliases: []string{"copilot-drive"},
		Short:   "Set the durable Copilot drive an agent relaunches on",
		Long: "Records which channel a Copilot agent's mail travels over — tmux send-keys, or the " +
			"API drive (`copilot --ui-server`, driven over JSON-RPC).\n\n" +
			"The API drive is opt-in per agent and is not yet verified in real use. This is its " +
			"rollback: `set-drive <agent> send-keys` records send-keys durably, so every later " +
			"launch of that agent stays on the channel every Copilot agent ran on before the drive " +
			"existed.\n\n" +
			"It is a PIN as well as a rollback. Recording send-keys for an agent that never chose " +
			"the drive is meaningful, because an unrecorded posture leaves a group or global " +
			"default profile free to turn the drive on at the next launch.\n\n" +
			"`send-keys` works with the daemon down, writing the record directly — a rollback that " +
			"needs a healthy daemon is no rollback at all. `api` requires the daemon, because only " +
			"agentd can produce the channel it claims. Both require the `agent.copilot-drive` " +
			"permission, which group ownership does NOT confer.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *SetDriveParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Agent).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.Drive).SetAlternatives([]string{setDriveSendKeys, setDriveAPI})
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *SetDriveParams, _ *cobra.Command, _ []string) {
			os.Exit(RunSetDrive(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// setDriveResp mirrors the daemon's copilot-drive payload.
type setDriveResp struct {
	ConvID  string `json:"conv_id"`
	AgentID string `json:"agent_id,omitempty"`
	Drive   string `json:"drive"`
	Record  string `json:"record"`
	Created bool   `json:"created,omitempty"`
	Changed bool   `json:"changed"`
	Live    bool   `json:"live,omitempty"`
}

// RunSetDrive records one agent's durable drive.
func RunSetDrive(p *SetDriveParams, stdout, stderr io.Writer) int {
	target := strings.TrimSpace(p.Agent)
	if target == "" {
		fmt.Fprintln(stderr, "Error: an agent selector is required.")
		return rcInvalidArg
	}
	drive := strings.TrimSpace(p.Drive)
	switch drive {
	case setDriveSendKeys, setDriveAPI:
	default:
		fmt.Fprintf(stderr, "Error: unknown drive %q (expected %s | %s).\n",
			p.Drive, setDriveSendKeys, setDriveAPI)
		return rcInvalidArg
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}

	// Prefer the daemon whenever it is up, in BOTH directions. While agentd is
	// running it is the single writer of the relaunch profile, and a second
	// writer in a second process makes the whole-blob hazard live: two
	// read-modify-writes with no lock between them lose one side's edit silently,
	// and the edit more likely to be lost is the operator's deliberate one.
	if DaemonAvailable() {
		if ask > 0 {
			fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
		}
		var resp setDriveResp
		if err := DaemonRequest(http.MethodPost, setDrivePath(target),
			map[string]any{"drive": drive}, &resp, DaemonOpts{AskHuman: ask}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		return printSetDrive(&resp, p.JSON, stdout, stderr)
	}

	// Escalation needs the daemon. Recording "api" against a dead daemon would
	// claim a channel nothing can create, and — because an API-driven
	// conversation HOLDS its mail rather than falling back to keystrokes — the
	// visible result would be an agent that quietly stops receiving messages.
	if drive == setDriveAPI {
		fmt.Fprintln(stderr, "Error: "+daemonRequiredMsg)
		fmt.Fprintln(stderr,
			"Note: only the API drive needs the daemon. `set-drive "+setDriveSendKeys+
				"` works with it down, which is the direction a rollback needs.")
		return rcIOFailure
	}
	return runSetDriveDirect(target, p.JSON, stdout, stderr)
}

// runSetDriveDirect is the daemon-down de-escalation, and the only path in this
// command that writes the database itself.
//
// The write is targeted rather than read-modify-write: it edits the one member
// inside SQLite and guards on the blob it read, so a daemon that comes up
// between the reachability probe and the write refuses the edit instead of
// silently overwriting whatever the daemon just composed. A probe is a reading,
// not a fact that stays true.
func runSetDriveDirect(target string, asJSON bool, stdout, stderr io.Writer) int {
	resolved, _, err := ResolveSelector(target)
	if err != nil || resolved == nil {
		fmt.Fprintf(stderr, "Error: could not resolve agent %q: %v\n", target, err)
		return rcNotFound
	}
	convID := resolved.ConvID
	// Gate the harness here as well as in the daemon. Without it this path is
	// Copilot-only by COINCIDENCE — a non-Copilot conversation has no drive
	// recorded, so it exits at the "nothing recorded" refusal below — and a
	// coincidence is not a check. It also means the two paths refuse the same
	// thing for the same stated reason rather than one refusing by accident.
	if h := recordedHarnessForConv(convID); h != "" && h != harness.CopilotName {
		fmt.Fprintf(stderr,
			"Error: the Copilot drive is a %s-only posture; conversation %s runs on %s.\n",
			harness.CopilotName, short(convID), h)
		return rcInvalidArg
	}
	drive, err := db.CopilotDriveTargetForConv(convID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	resp := setDriveResp{ConvID: convID, AgentID: drive.AgentID, Drive: setDriveSendKeys}

	// Nothing recorded, and no daemon to decide the policy question of bringing a
	// record into existence — the seeds need the conversation's harness and cwd,
	// which is the daemon's resolution rather than this process's. Refuse, naming
	// what to do, instead of inventing a record.
	if drive.Record == db.CopilotDriveRecordNone {
		fmt.Fprintf(stderr,
			"Error: nothing records a Copilot drive for %s, so there is nothing to turn off "+
				"here.\nStart the daemon (tclaude agentd serve) and run this again to PIN "+
				"send-keys — that is what stops a group or global default profile turning the "+
				"drive on at the next launch.\n", short(convID))
		// Nothing found to edit, which is the same shape the daemon's own
		// refusal has and maps to the same code a not-found does there.
		return rcNotFound
	}
	if !drive.Value {
		resp.Record = string(drive.Record)
		return printSetDrive(&resp, asJSON, stdout, stderr)
	}

	var ok bool
	switch drive.Record {
	case db.CopilotDriveRecordAgentProfile:
		ok, err = db.CompareAndSetAgentCopilotAPI(drive.AgentID, false, drive.Raw)
	case db.CopilotDriveRecordConversationFallback:
		ok, err = db.CompareAndSetConversationCopilotAPI(convID, false, drive.Raw)
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if !ok {
		fmt.Fprintf(stderr,
			"Error: the %s for %s changed while the drive was being written, so nothing was "+
				"written.\nThe daemon may have come up; read it again and retry.\n",
			drive.Record, short(convID))
		// Same code the daemon path yields for a 409: not the operator's mistake,
		// and a retry is the response.
		return rcIOFailure
	}
	resp.Record = string(drive.Record)
	resp.Changed = true
	return printSetDrive(&resp, asJSON, stdout, stderr)
}

// recordedHarnessForConv reads the harness from the conversation's own durable
// resume record, which is the harness a relaunch would use and is readable with
// the daemon down — the only condition this path runs under. An unreadable or
// absent record answers "" (unknown) and does not gate: the "nothing records a
// drive" refusal below is the honest answer for that state, and refusing on a
// guess would be worse than refusing on a fact.
func recordedHarnessForConv(convID string) string {
	profile, err := db.ConversationResumeProfileForConv(convID)
	if err != nil || profile == nil {
		return ""
	}
	return strings.TrimSpace(profile.Harness)
}

func setDrivePath(target string) string {
	return "/v1/agent/" + url.PathEscape(target) + "/copilot-drive"
}

// printSetDrive renders the outcome.
//
// Four facts, and each is here because leaving it out produces an operator who
// believes something false:
//
//   - WHICH RECORD took the write, because two shapes of "durably off" look
//     identical otherwise and only one of them can be outvoted later.
//   - CREATED versus EDITED, because "created" tells the operator nothing was
//     recorded before — which is itself the diagnostic for a lower tier having
//     been the thing speaking for this agent.
//   - The FUTURE-MEMBER boundary, because a pin is per-agent on a record, and an
//     operator who pins one member and expects the group's next spawn to come up
//     on send-keys will be wrong, quietly.
//   - The LIVE channel, because a pin is durable immediately and does not
//     redirect a channel that is already up: routing answers from the live handle
//     first and reads the record only when there is no handle.
func printSetDrive(resp *setDriveResp, asJSON bool, stdout, stderr io.Writer) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcIOFailure
		}
		return rcOK
	}
	label := short(resp.ConvID)
	verb := "edited"
	if resp.Created {
		verb = "CREATED"
	}
	switch {
	case !resp.Changed && resp.Drive == "":
		fmt.Fprintf(stdout, "%s: no record says which Copilot drive this conversation takes\n", label)
		return rcOK
	case !resp.Changed:
		fmt.Fprintf(stdout, "%s: Copilot drive already %s (%s, unchanged)\n",
			label, resp.Drive, resp.Record)
	default:
		fmt.Fprintf(stdout, "%s: Copilot drive → %s (%s the %s)\n",
			label, resp.Drive, verb, resp.Record)
	}
	if resp.Created {
		fmt.Fprintln(stdout,
			"  nothing recorded a drive for this agent before, so a default profile was free "+
				"to answer for it")
	}
	if resp.Drive == setDriveSendKeys && resp.Changed {
		fmt.Fprintln(stdout,
			"  this pins THIS agent, not future members of its group — a group default "+
				"profile is the lever for those")
	}
	if resp.Live {
		// Naming the restart is not a detail. Routing answers from the live handle
		// first, so this pane keeps its channel — but an agentd restart drops every
		// handle, and the reconnect sweep now declines to re-acquire a drive an
		// operator turned off. So the pin starts biting at a restart, with no
		// relaunch and no channel "ending" in any sense the operator can observe.
		// Saying only "until that channel ends" would describe the wrong event.
		fmt.Fprintln(stdout,
			"  durable now; this conversation keeps its current API channel until that "+
				"channel ends or agentd restarts (a restart does not re-acquire a drive "+
				"you turned off), so relaunch it if you want the change to bite immediately")
	}
	return rcOK
}
