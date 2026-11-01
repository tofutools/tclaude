package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent steward` — the operator hands the Steward role to a running
// agent. Not `promote`, which enrols a plain conversation as an agent. Every
// command here is the human's alone: the daemon refuses an agent, with no
// permission or sudo path.

const stewardNeeds = "Only the human operator can run this; agents are refused. Needs Collab."

func stewardCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "steward",
		Short:       "Choose which agent is the Steward (human only)",
		Long:        "One agent is the active Steward: it gets the \"steward\" address, the Config name and the companion. Former Stewards can stand by on the roster, without Steward access.\n\n" + stewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			stewardLsCmd(), stewardSetCmd(), stewardStepDownCmd(), stewardRemoveCmd(), stewardExternalCmd(),
		},
	}.ToCobra()
}

type stewardRosterRow struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	Seat    string `json:"seat"`
	Online  bool   `json:"online"`
	Harness string `json:"harness"`
}

type stewardLsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func stewardLsCmd() *cobra.Command {
	return boa.CmdT[stewardLsParams]{
		Use:         "ls",
		Short:       "List the active Steward and the roster",
		Long:        stewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *stewardLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runStewardLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runStewardLs(p *stewardLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var raw json.RawMessage
	if err := DaemonGet("/v1/steward", &raw); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		_, _ = stdout.Write(raw)
		fmt.Fprintln(stdout)
		return rcOK
	}
	var resp struct {
		Roster []stewardRosterRow `json:"roster"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if len(resp.Roster) == 0 {
		fmt.Fprintln(stdout, "No Steward. Use `tclaude agent steward set <agent>`, or summon one from the companion window.")
		return rcOK
	}
	for _, row := range resp.Roster {
		state := "offline"
		if row.Online {
			state = "running"
		}
		fmt.Fprintf(stdout, "%-9s  %-7s  %-8s  %s  %s\n", row.Seat, state, row.Harness, row.AgentID, row.Name)
	}
	return rcOK
}

type stewardSetParams struct {
	Agent string `pos:"true" help:"The agent to make the Steward"`
	Then  string `long:"then" optional:"true" default:"roster" help:"What becomes of the current Steward: roster, keep (a normal agent) or retire"`
	Yes   bool   `long:"yes" help:"Make the change; without it, only show what would change"`
}

func stewardSetCmd() *cobra.Command {
	return boa.CmdT[stewardSetParams]{
		Use:         "set",
		Short:       "Make a running agent the Steward",
		Long:        "Shows what changes, and with --yes makes the change. The agent keeps its conversation, harness, working directory and what it already held, and gets the Steward's access and brief. The current Steward loses what the role gave it.\n\n" + stewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *stewardSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runStewardSet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type stewardPreviewView struct {
	Name          string   `json:"name"`
	Harness       string   `json:"harness"`
	Online        bool     `json:"online"`
	Refused       string   `json:"refused"`
	AlreadyActive bool     `json:"already_active"`
	Gains         []string `json:"gains"`
	Extra         struct {
		Standing  []string `json:"standing"`
		Inherited []string `json:"inherited"`
	} `json:"extra"`
	ExtraSudo []string `json:"extra_sudo"`
	Denied    []string `json:"denied"`
	Withheld  []string `json:"withheld"`
	Notes     []string `json:"notes"`
	Previous  *struct {
		Name  string   `json:"name"`
		Loses []string `json:"loses"`
		Keeps []string `json:"keeps"`
	} `json:"previous"`
}

func printStewardPreview(w io.Writer, v *stewardPreviewView, then string) {
	line := func(label string, slugs []string) {
		if len(slugs) > 0 {
			fmt.Fprintf(w, "  %s: %s\n", label, strings.Join(slugs, ", "))
		}
	}
	fmt.Fprintf(w, "%s (%s) would become the Steward.\n", v.Name, v.Harness)
	line("Gains", v.Gains)
	line("Already holds, and a Steward could use", append(append(append([]string{}, v.Extra.Standing...), v.Extra.Inherited...), v.ExtraSudo...))
	line("Stays denied", v.Denied)
	line("Stays off, as you left it", v.Withheld)
	for _, note := range v.Notes {
		fmt.Fprintf(w, "  %s\n", note)
	}
	if v.Previous != nil {
		fmt.Fprintf(w, "%s stops being the Steward (then: %s).\n", v.Previous.Name, then)
		line("Loses", v.Previous.Loses)
		line("Keeps", v.Previous.Keeps)
	}
}

func runStewardSet(p *stewardSetParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var preview stewardPreviewView
	if err := DaemonGet("/v1/steward/handover?agent="+url.QueryEscape(p.Agent), &preview); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if preview.Refused != "" {
		fmt.Fprintf(stderr, "Error: %s\n", preview.Refused)
		return rcIOFailure
	}
	if preview.AlreadyActive {
		fmt.Fprintf(stdout, "%s is already the active Steward.\n", preview.Name)
		return rcOK
	}
	then := p.Then
	if then == "" {
		then = "roster"
	}
	printStewardPreview(stdout, &preview, then)
	if !p.Yes {
		fmt.Fprintln(stdout, "Run again with --yes to make the change.")
		return rcOK
	}
	var resp struct {
		Name    string `json:"name"`
		Warning string `json:"warning"`
	}
	if err := DaemonPost("/v1/steward/handover", map[string]any{"agent": p.Agent, "then": then}, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s is now the Steward.\n", resp.Name)
	if resp.Warning != "" {
		fmt.Fprintf(stderr, "Warning: %s\n", resp.Warning)
	}
	return rcOK
}

type stewardStepDownParams struct {
	Agent string `pos:"true" optional:"true" help:"The Steward to stand down; default the active one"`
	Then  string `long:"then" optional:"true" default:"roster" help:"roster, or keep to leave it a normal agent"`
}

func stewardStepDownCmd() *cobra.Command {
	return boa.CmdT[stewardStepDownParams]{
		Use:         "step-down",
		Short:       "Stand a Steward down, with nobody in its place",
		Long:        "The agent loses what the role gave it, and keeps what you granted it separately. It keeps running.\n\n" + stewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *stewardStepDownParams, _ *cobra.Command, _ []string) {
			os.Exit(stewardPost("/v1/steward/step-down", map[string]any{"agent": p.Agent, "then": p.Then}, "Stood down.", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type stewardRemoveParams struct {
	Agent string `pos:"true" help:"The agent to take off the roster"`
}

func stewardRemoveCmd() *cobra.Command {
	return boa.CmdT[stewardRemoveParams]{
		Use:         "rm",
		Short:       "Take a standby off the roster",
		Long:        "It stays a normal agent.\n\n" + stewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *stewardRemoveParams, _ *cobra.Command, _ []string) {
			os.Exit(stewardPost("/v1/steward/roster/remove", map[string]any{"agent": p.Agent}, "Removed from the roster.", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func stewardPost(path string, body any, done string, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if err := DaemonPost(path, body, nil); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintln(stdout, done)
	return rcOK
}
