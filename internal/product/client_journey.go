package product

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
)

type journeyCall func(*cobra.Command, string, string, any) error

func registerJourney(root *cobra.Command, call journeyCall) {
	history := boa.CmdT[struct{}]{Use: "history", Short: "Find and read authorized earlier work"}.ToCobra()
	var harness, workspace, query string
	var archived bool
	search := boa.CmdT[struct{}]{Use: "search", Short: "Search catalogued histories with source coverage"}.ToCobra()
	search.Args = cobra.NoArgs
	search.Flags().StringVar(&harness, "harness", "", "Filter by harness")
	search.Flags().StringVar(&workspace, "workspace", "", "Filter by workspace ID")
	search.Flags().StringVar(&query, "query", "", "Search text")
	search.Flags().BoolVar(&archived, "archived", false, "Filter by archived state")
	search.RunE = func(cmd *cobra.Command, _ []string) error {
		body := map[string]any{"harness": harness, "workspace_id": workspace, "query": query}
		if cmd.Flags().Changed("archived") {
			body["archived"] = archived
		}
		return call(cmd, "POST", "/v2/history/search", body)
	}
	refresh := boa.CmdT[struct{}]{Use: "refresh SOURCE", Short: "Refresh an explicitly configured history source"}.ToCobra()
	refresh.Args = cobra.ExactArgs(1)
	var refreshHarness string
	refresh.Flags().StringVar(&refreshHarness, "harness", "", "Provider of the configured source")
	_ = refresh.MarkFlagRequired("harness")
	refresh.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "POST", "/v2/history/refresh", map[string]string{"harness": refreshHarness, "source": args[0]})
	}
	var conversationRevision, pointRevision uint64
	var point string
	read := boa.CmdT[struct{}]{Use: "read CONVERSATION", Short: "Read a catalogued history at its selected revision"}.ToCobra()
	read.Args = cobra.ExactArgs(1)
	read.Flags().Uint64Var(&conversationRevision, "revision", 0, "Conversation revision returned by search")
	read.Flags().StringVar(&point, "point", "", "Optional history point ID returned by read")
	read.Flags().Uint64Var(&pointRevision, "point-revision", 0, "Selected point revision")
	_ = read.MarkFlagRequired("revision")
	read.MarkFlagsRequiredTogether("point", "point-revision")
	read.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "POST", "/v2/history/read", map[string]any{"selection": map[string]any{"ConversationID": args[0], "ExpectedConversationRevision": conversationRevision, "PointID": point, "ExpectedPointRevision": pointRevision}})
	}
	history.AddCommand(search, refresh, read, journeyFileCommand("metadata", "Set a conversation title and archive state at an expected revision", "/v2/history/metadata", call))
	root.AddCommand(history)

	workspaceCmd := boa.CmdT[struct{}]{Use: "workspace", Short: "Manage explicit workspace ownership and retained checkouts"}.ToCobra()
	for _, verb := range []string{"register", "create"} {
		var requestID, intentFile string
		c := boa.CmdT[struct{}]{Use: verb + " ID", Short: verb + " a workspace from explicit intent"}.ToCobra()
		c.Args = cobra.ExactArgs(1)
		c.Flags().StringVar(&requestID, "request-id", "", "Stable identity for this exact operation")
		c.Flags().StringVar(&intentFile, "intent-file", "", "Workspace intent JSON file")
		_ = c.MarkFlagRequired("request-id")
		_ = c.MarkFlagRequired("intent-file")
		c.RunE = func(cmd *cobra.Command, args []string) error {
			intent, err := readJourneyJSON(intentFile)
			if err != nil {
				return err
			}
			return call(cmd, "POST", "/v2/workspaces/"+verb, map[string]any{"id": args[0], "request_id": requestID, "intent": intent})
		}
		workspaceCmd.AddCommand(c)
	}
	inspectWorkspace := boa.CmdT[struct{}]{Use: "inspect ID", Short: "Read workspace state and its current revision"}.ToCobra()
	inspectWorkspace.Args = cobra.ExactArgs(1)
	inspectWorkspace.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "GET", "/v2/workspaces/"+url.PathEscape(args[0]), nil)
	}
	var removalID string
	var workspaceRevision uint64
	var destructive bool
	remove := boa.CmdT[struct{}]{Use: "remove ID", Short: "Explicitly remove an owned checkout"}.ToCobra()
	remove.Args = cobra.ExactArgs(1)
	remove.Flags().StringVar(&removalID, "request-id", "", "Stable identity for this exact removal")
	remove.Flags().Uint64Var(&workspaceRevision, "revision", 0, "Workspace revision from inspect")
	remove.Flags().BoolVar(&destructive, "destructive", false, "Request separately authorized destructive removal")
	_ = remove.MarkFlagRequired("request-id")
	_ = remove.MarkFlagRequired("revision")
	remove.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "POST", "/v2/workspaces/remove", map[string]any{"request_id": removalID, "workspace_id": args[0], "expected_revision": workspaceRevision, "destructive": destructive})
	}
	workspaceCmd.AddCommand(inspectWorkspace, remove, journeyFileCommand("restore", "Restore a retained owned checkout at an expected revision", "/v2/workspaces/restore", call))
	root.AddCommand(workspaceCmd)
	var shellRequest, shellSandbox string
	var shellRevision uint64
	shell := boa.CmdT[struct{}]{Use: "shell WORKSPACE", Short: "Start an interactive shell without creating an agent or conversation"}.ToCobra()
	shell.Args = cobra.ExactArgs(1)
	shell.Flags().StringVar(&shellRequest, "request-id", "", "Stable identity for this shell launch")
	shell.Flags().Uint64Var(&shellRevision, "revision", 0, "Workspace revision returned by inspect")
	shell.Flags().StringVar(&shellSandbox, "sandbox", "", "Explicit supported confinement policy")
	_ = shell.MarkFlagRequired("request-id")
	_ = shell.MarkFlagRequired("revision")
	_ = shell.MarkFlagRequired("sandbox")
	shell.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "POST", "/v2/shells", map[string]any{"request_id": shellRequest, "workspace_id": args[0], "expected_revision": shellRevision, "sandbox": shellSandbox})
	}
	root.AddCommand(shell)

	work := boa.CmdT[struct{}]{Use: "work", Short: "Run bounded assignments and record explicit outcomes"}.ToCobra()
	var startID, specFile string
	start := boa.CmdT[struct{}]{Use: "start ID", Short: "Start work from a pinned source, configuration and outcome policy"}.ToCobra()
	start.Args = cobra.ExactArgs(1)
	start.Flags().StringVar(&startID, "request-id", "", "Stable identity for this exact assignment")
	start.Flags().StringVar(&specFile, "spec-file", "", "Work specification JSON file")
	_ = start.MarkFlagRequired("request-id")
	_ = start.MarkFlagRequired("spec-file")
	start.RunE = func(cmd *cobra.Command, args []string) error {
		spec, err := readJourneyJSON(specFile)
		if err != nil {
			return err
		}
		return call(cmd, "POST", "/v2/work", map[string]any{"request_id": startID, "id": args[0], "spec": spec})
	}
	inspectWork := boa.CmdT[struct{}]{Use: "inspect ID", Short: "Read work attempts, evidence, outcome and revisions"}.ToCobra()
	inspectWork.Args = cobra.ExactArgs(1)
	inspectWork.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "GET", "/v2/work/"+url.PathEscape(args[0]), nil)
	}
	work.AddCommand(start, inspectWork)
	registerWorkSettlement(work, call)
	root.AddCommand(work)
}

func registerWorkSettlement(work *cobra.Command, call journeyCall) {
	var evidenceFile string
	evidence := boa.CmdT[struct{}]{Use: "evidence", Short: "Record evidence attributed to the authenticated caller"}.ToCobra()
	evidence.Args = cobra.NoArgs
	evidence.Flags().StringVar(&evidenceFile, "file", "", "Evidence JSON: request_id, work run, expected revision, exact attempt and result")
	_ = evidence.MarkFlagRequired("file")
	evidence.RunE = func(cmd *cobra.Command, _ []string) error {
		body, err := readJourneyJSON(evidenceFile)
		if err != nil {
			return err
		}
		return call(cmd, "POST", "/v2/work/evidence", body)
	}
	var decisionFile string
	decision := boa.CmdT[struct{}]{Use: "decide", Short: "Submit an authorized outcome decision for an exact attempt"}.ToCobra()
	decision.Args = cobra.NoArgs
	decision.Flags().StringVar(&decisionFile, "file", "", "Decision JSON: request_id, work run, expected revision, attempt, decision and reason")
	_ = decision.MarkFlagRequired("file")
	decision.RunE = func(cmd *cobra.Command, _ []string) error {
		body, err := readJourneyJSON(decisionFile)
		if err != nil {
			return err
		}
		return call(cmd, "POST", "/v2/work/decision", body)
	}
	var requestID, reason string
	var revision uint64
	cancel := boa.CmdT[struct{}]{Use: "cancel ID", Short: "Cancel future work effects and account for already admitted effects"}.ToCobra()
	cancel.Args = cobra.ExactArgs(1)
	cancel.Flags().StringVar(&requestID, "request-id", "", "Stable identity for this cancellation")
	cancel.Flags().Uint64Var(&revision, "revision", 0, "Work revision from inspect")
	cancel.Flags().StringVar(&reason, "reason", "", "Reason for cancellation")
	_ = cancel.MarkFlagRequired("request-id")
	_ = cancel.MarkFlagRequired("revision")
	_ = cancel.MarkFlagRequired("reason")
	cancel.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "POST", "/v2/work/cancel", map[string]any{"request_id": requestID, "work_run_id": args[0], "expected_revision": revision, "reason": reason})
	}
	work.AddCommand(evidence, decision, cancel, journeyFileCommand("resolve", "Operator confirmation that an uncertain effect did not occur; never retries", "/v2/work/resolve", call))
}

func readJourneyJSON(path string) (json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit || !json.Valid(data) {
		return nil, fmt.Errorf("input must be valid JSON no larger than 1 MiB")
	}
	return json.RawMessage(data), nil
}

func journeyFileCommand(name, description, path string, call journeyCall) *cobra.Command {
	var file string
	cmd := boa.CmdT[struct{}]{Use: name, Short: description}.ToCobra()
	cmd.Args = cobra.NoArgs
	cmd.Flags().StringVar(&file, "file", "", "Request JSON including request_id and expected_revision")
	_ = cmd.MarkFlagRequired("file")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		body, err := readJourneyJSON(file)
		if err != nil {
			return err
		}
		return call(cmd, "POST", path, body)
	}
	return cmd
}
