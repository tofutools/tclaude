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
	"strings"

	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/worklist"
)

func handleProcessWorklist(w http.ResponseWriter, r *http.Request) {
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	items, err := loadProcessWorklist(r.Context(), fs)
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
	writeProcessJSON(w, http.StatusOK, map[string]any{"items": worklist.ApplyFilter(items, filter)})
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
	body.Action = strings.ToLower(strings.TrimSpace(body.Action))
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
	items, err := loadProcessWorklist(r.Context(), fs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_worklist", err.Error())
		return
	}
	item, found := worklist.Find(items, r.PathValue("itemId"))
	if !found {
		http.NotFound(w, r)
		return
	}
	if !isHuman && item.Assignee != string(actor) {
		writeError(w, http.StatusForbidden, "forbidden", "agent caller is not the assignee for this work item")
		return
	}
	if !slices.Contains(item.AvailableActions, body.Action) {
		writeError(w, http.StatusConflict, "process_action", fmt.Sprintf("action %q is not available for item %q", body.Action, item.ID))
		return
	}

	evidenceRef := worklistActionEvidence(item.ID, body)
	executor := processexec.New(fs, nil)
	if item.Target.Blocked {
		snapshot, loadErr := fs.LoadRun(r.Context(), item.Run)
		if loadErr != nil {
			writeProcessActionError(w, loadErr)
			return
		}
		request, bindErr := processexec.BindBlockResolution(snapshot, processexec.BlockResolutionRequest{
			RunID: item.Run, NodeID: item.Node, Decision: state.BlockDecision(body.Action),
			Actor: actor, Reason: body.Comment, EvidenceRef: evidenceRef,
		})
		if bindErr != nil {
			writeProcessActionError(w, bindErr)
			return
		}
		if _, err = executor.ResolveBlocked(r.Context(), request); err != nil {
			writeProcessActionError(w, err)
			return
		}
	} else {
		verdict, verdictErr := worklistObservationVerdict(r.Context(), fs, item, body.Action)
		if verdictErr != nil {
			writeProcessActionError(w, verdictErr)
			return
		}
		_, err = executor.RecordOutstandingObservation(r.Context(), item.Run, item.Target.CommandID, processexec.Observation{
			Actor: actor, Verdict: verdict, Feedback: body.Comment, EvidenceRef: evidenceRef,
		})
		if err != nil {
			writeProcessActionError(w, err)
			return
		}
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"recorded": true, "itemId": item.ID, "actor": actor})
}

func loadProcessWorklist(ctx context.Context, fs *store.FS) ([]worklist.Item, error) {
	runs, err := fs.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	snapshots := make([]store.Snapshot, 0, len(runs))
	for _, run := range runs {
		if run.ID == "" {
			continue
		}
		snapshot, loadErr := fs.LoadRun(ctx, run.ID)
		if loadErr != nil {
			return nil, fmt.Errorf("load run %q: %w", run.ID, loadErr)
		}
		snapshots = append(snapshots, snapshot)
	}
	return worklist.Derive(snapshots), nil
}

func worklistObservationVerdict(ctx context.Context, fs *store.FS, item worklist.Item, action string) (string, error) {
	snapshot, err := fs.LoadRun(ctx, item.Run)
	if err != nil {
		return "", err
	}
	command, ok := snapshot.State.OutstandingCommands[item.Target.CommandID]
	if !ok {
		return "", fmt.Errorf("process command %q is not present", item.Target.CommandID)
	}
	if command.Kind == state.CommandKindRecordDecision {
		return action, nil
	}
	switch action {
	case "approve":
		return "pass", nil
	case "reject", "ask-changes":
		return "fail", nil
	default:
		return action, nil
	}
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
