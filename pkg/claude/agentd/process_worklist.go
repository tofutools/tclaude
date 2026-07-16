package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/worklist"
)

func handleProcessWorklist(w http.ResponseWriter, r *http.Request) {
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	result, err := loadProcessWorklist(r.Context(), fs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_worklist", err.Error())
		return
	}
	filter := worklist.Filter{
		Assignee: strings.TrimSpace(r.URL.Query().Get("assignee")),
		Kind:     worklist.Kind(strings.TrimSpace(r.URL.Query().Get("kind"))),
		Run:      strings.TrimSpace(r.URL.Query().Get("run")),
		Status:   state.WaitStatus(strings.TrimSpace(r.URL.Query().Get("status"))),
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{
		"items": worklist.ApplyFilter(result.Items, filter), "degradedRuns": result.DegradedRuns,
	})
}

type processWorklistActionRequest struct {
	Action         string `json:"action"`
	Comment        string `json:"comment"`
	IdempotencyKey string `json:"idempotencyKey"`
}

func handleProcessWorklistAction(w http.ResponseWriter, r *http.Request) {
	callerConv, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	actor := state.ActorRef("human:operator")
	if !isHuman {
		agentID := peerAgentID(callerConv)
		if agentID == "" {
			writeError(w, http.StatusForbidden, "forbidden", "caller has no stable agent identity")
			return
		}
		actor = state.ActorRef("agent:" + agentID)
	}

	var body processWorklistActionRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	body.Action = strings.TrimSpace(body.Action)
	body.Comment = strings.TrimSpace(body.Comment)
	body.IdempotencyKey = strings.TrimSpace(body.IdempotencyKey)
	if body.Action == "" || len(body.Action) > 64 || body.Comment == "" || len(body.Comment) > 10_000 ||
		body.IdempotencyKey == "" || len(body.IdempotencyKey) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "action, comment, and idempotencyKey are required and must be within their size limits")
		return
	}

	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	result, err := loadProcessWorklist(r.Context(), fs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_worklist", err.Error())
		return
	}
	item, found := worklist.Find(result.Items, r.PathValue("itemId"))
	if !found {
		http.NotFound(w, r)
		return
	}
	if !isHuman && item.Assignee != string(actor) {
		writeError(w, http.StatusForbidden, "forbidden", "agent caller is not the assignee for this work item")
		return
	}
	if item.Kind == worklist.KindAgentObligation {
		writeError(w, http.StatusConflict, "process_action", "agent obligations must be reported through the process run/node report route with a durable evidence ref")
		return
	}
	canonicalAction, available := processexec.CanonicalObligationAction(item.AvailableActions, body.Action)
	if !available {
		writeError(w, http.StatusConflict, "process_action", fmt.Sprintf("action %q is not available for item %q", body.Action, item.ID))
		return
	}
	body.Action = canonicalAction

	evidenceRef := worklistActionEvidence(item.ID, body)
	executor := processexec.New(fs, nil)
	if item.Target.Blocked {
		snapshot, loadErr := fs.LoadRun(r.Context(), item.Run)
		if loadErr != nil {
			writeProcessActionError(w, loadErr)
			return
		}
		request, bindErr := bindWorklistBlockResolution(snapshot, item, body, actor, evidenceRef)
		if bindErr != nil {
			writeProcessActionError(w, bindErr)
			return
		}
		if _, err = executor.ResolveBlocked(r.Context(), request); err != nil {
			writeProcessActionError(w, err)
			return
		}
	} else {
		schema, schemaErr := supportedProcessRunSchema(r.Context(), fs, item.Run)
		if schemaErr != nil {
			writeProcessActionError(w, schemaErr)
			return
		}
		observation := processexec.Observation{Actor: actor, Verdict: body.Action, Feedback: body.Comment, EvidenceRef: evidenceRef}
		if schema == pathv1.CheckpointStateSchemaVersion {
			_, err = processexec.NewExclusiveV7(fs, nil).RecordObservation(r.Context(), item.Run, item.Node, item.Target.CommandID, observation)
		} else {
			_, err = executor.RecordOutstandingObservation(r.Context(), item.Run, item.Target.CommandID, observation)
		}
		if err != nil {
			writeProcessActionError(w, err)
			return
		}
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"recorded": true, "itemId": item.ID, "actor": actor})
}

func bindWorklistBlockResolution(snapshot store.Snapshot, item worklist.Item, body processWorklistActionRequest, actor state.ActorRef, evidenceRef string) (processexec.BlockResolutionRequest, error) {
	return processexec.BindBlockResolution(snapshot, processexec.BlockResolutionRequest{
		RunID: item.Run, NodeID: item.Node, BlockedAttempt: item.Attempt, Decision: state.BlockDecision(body.Action),
		Actor: actor, Reason: body.Comment, EvidenceRef: evidenceRef,
	})
}

type processWorklistLoadResult struct {
	Items        []worklist.Item `json:"items"`
	DegradedRuns []degradedRun   `json:"degradedRuns"`
}

type degradedRun struct {
	Run   string `json:"run"`
	Error string `json:"error"`
}

func loadProcessWorklist(ctx context.Context, fs *store.FS) (processWorklistLoadResult, error) {
	runs, err := fs.ListRuns(ctx)
	if err != nil {
		return processWorklistLoadResult{}, err
	}
	snapshots := make([]store.Snapshot, 0, len(runs))
	v7Items := make([]worklist.Item, 0)
	degraded := make([]degradedRun, 0)
	for _, run := range runs {
		if run.ID == "" {
			continue
		}
		schema, schemaErr := supportedProcessRunSchema(ctx, fs, run.ID)
		if schemaErr != nil {
			degraded = append(degraded, degradedRun{Run: run.ID, Error: schemaErr.Error()})
			continue
		}
		if schema == pathv1.CheckpointStateSchemaVersion {
			items, itemErr := pathV1WorklistItems(ctx, fs, run.ID)
			if itemErr != nil {
				degraded = append(degraded, degradedRun{Run: run.ID, Error: itemErr.Error()})
				continue
			}
			v7Items = append(v7Items, items...)
			continue
		}
		snapshot, loadErr := fs.LoadRun(ctx, run.ID)
		if loadErr != nil {
			degraded = append(degraded, degradedRun{Run: run.ID, Error: loadErr.Error()})
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	items := append(worklist.Derive(snapshots), v7Items...)
	slices.SortFunc(items, func(a, b worklist.Item) int { return strings.Compare(a.ID, b.ID) })
	return processWorklistLoadResult{Items: items, DegradedRuns: degraded}, nil
}

func pathV1WorklistItems(ctx context.Context, fs *store.FS, runID string) ([]worklist.Item, error) {
	snapshot, err := fs.LoadPathV1RunView(ctx, runID)
	if err != nil {
		return nil, err
	}
	input, err := pathv1.VerifyExclusiveInput(ctx, snapshot.CheckpointJSON, snapshot.TemplateSource)
	if err != nil {
		return nil, err
	}
	parsed, err := model.Parse(snapshot.TemplateSource)
	if err != nil || parsed.Diagnostics.HasErrors() {
		return nil, fmt.Errorf("schema-7 worklist template is invalid")
	}
	_ = input // verification above is the authority for every projected record.
	aggregate, err := pathv1.CurrentAggregateCheckpoint(snapshot.Checkpoint)
	if err != nil {
		return nil, err
	}
	settled := make(map[string]bool)
	for _, command := range aggregate.Commands {
		if command.Identity.Kind == pathv1.CommandSettleAttempt && (command.State == pathv1.CommandObserved || command.State == pathv1.CommandReconciled) {
			settled[command.Identity.InputDigest] = true
		}
	}
	items := make([]worklist.Item, 0)
	for _, command := range aggregate.Commands {
		if command.Identity.Kind != pathv1.CommandPerformAttempt {
			continue
		}
		status := state.WaitStatusPending
		if command.State == pathv1.CommandObserved || command.State == pathv1.CommandReconciled {
			if !settled[command.ID] {
				continue
			}
			status = state.WaitStatusSatisfied
		} else if !command.State.Active() {
			continue
		}
		activation, ok := aggregate.Routing.Activations[command.Identity.SourceActivationID]
		if !ok {
			continue
		}
		reservation, ok := aggregate.Routing.Reservations[activation.ReservationID]
		node := parsed.Template.Nodes[reservation.NodeID]
		if !ok || node.Performer == nil || (node.Type != model.NodeTypeTask && node.Type != model.NodeTypeDecision) {
			continue
		}
		performer := model.InterpolatePerformer(*node.Performer, snapshot.Run.Params)
		projected, buildErr := buildPathV1WorklistItem(runID, parsed.Template, reservation.NodeID, &performer, command, status)
		if buildErr != nil {
			return nil, buildErr
		}
		items = append(items, projected...)
	}
	return items, nil
}

func buildPathV1WorklistItem(runID string, tmpl *model.Template, nodeID string, performer *model.Performer, command pathv1.CommandRecord, status state.WaitStatus) ([]worklist.Item, error) {
	if performer == nil || tmpl == nil {
		return nil, fmt.Errorf("schema-7 worklist performer is absent")
	}
	node := tmpl.Nodes[nodeID]
	commandID := "cmd_" + command.ID[:24]
	kind := worklist.KindAgentObligation
	assignee := ""
	summary := strings.TrimSpace(performer.Prompt)
	actions := []string{"pass", "fail", "ask-changes"}
	if performer.Kind == model.PerformerHuman {
		kind = worklist.KindHumanWait
		assignee = strings.TrimSpace(performer.Assignee)
		if assignee == "" {
			assignee = strings.TrimSpace(performer.Profile)
		}
		if assignee == "" {
			assignee = "human:operator"
		} else if !strings.HasPrefix(assignee, "human:") && !strings.HasPrefix(assignee, "role:") {
			assignee = "human:" + assignee
		}
		summary = strings.TrimSpace(performer.Ask)
		if summary == "" {
			summary = strings.TrimSpace(performer.Prompt)
		}
		actions = []string{"approve", "reject", "ask-changes"}
		if node.Type == model.NodeTypeDecision {
			kind = worklist.KindDecisionNeeded
			actions = make([]string, 0, len(node.Next))
			for outcome := range node.Next {
				actions = append(actions, outcome)
			}
			slices.Sort(actions)
		} else if len(performer.Choices) > 0 {
			actions = append([]string(nil), performer.Choices...)
		}
	} else if agent, lookupErr := db.AgentForProcessCommand(commandID); lookupErr != nil {
		return nil, lookupErr
	} else if agent != nil {
		assignee = "agent:" + agent.AgentID
	}
	attemptNumber := int(command.Identity.Attempt)
	stable := sha256.Sum256([]byte(runID + "\x00" + nodeID + "\x00" + commandID + "\x00" + strconv.Itoa(attemptNumber)))
	return []worklist.Item{{
		ID: "wi_" + hex.EncodeToString(stable[:12]), Run: runID, Node: nodeID, Attempt: attemptNumber,
		Kind: kind, Assignee: assignee, Status: status, Summary: summary,
		AvailableActions: actions, Links: worklist.Links{RunID: runID, NodeID: nodeID},
		Target: worklist.ActionTarget{CommandID: commandID},
	}}, nil
}

func worklistActionEvidence(itemID string, body processWorklistActionRequest) string {
	sum := sha256.Sum256([]byte(itemID + "\x00" + body.IdempotencyKey + "\x00" + body.Action + "\x00" + body.Comment))
	return "worklist-action:sha256:" + hex.EncodeToString(sum[:])
}

func writeProcessActionError(w http.ResponseWriter, err error) {
	status := http.StatusConflict
	if errors.Is(err, store.ErrNotFound) {
		status = http.StatusNotFound
	}
	writeError(w, status, "process_action", err.Error())
}
