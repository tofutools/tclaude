package agentd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const boardMaxStepsPerPatch = 100

// validBoardSteps keeps the steps whose ids could name an element. Anything
// else in a step waits for boardRestore, which skips what it cannot use.
func validBoardSteps(steps [][]db.CollabBoardStepElement) [][]db.CollabBoardStepElement {
	if len(steps) > boardMaxStepsPerPatch {
		steps = steps[len(steps)-boardMaxStepsPerPatch:]
	}
	out := make([][]db.CollabBoardStepElement, 0, len(steps))
	for _, step := range steps {
		kept := make([]db.CollabBoardStepElement, 0, len(step))
		for _, each := range step {
			if elementIDPattern.MatchString(each.ID) {
				kept = append(kept, each)
			}
		}
		if len(kept) > 0 {
			out = append(out, kept)
		}
	}
	return out
}

// boardFingerprint identifies what an element looks like, leaving out what
// each write changes anyway. It is empty for anything that is not an element.
func boardFingerprint(body json.RawMessage) string {
	if len(body) == 0 || string(body) == "null" {
		return ""
	}
	normalized, err := normalizeBoardElement(body, "")
	if err != nil {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(normalized.Body, &fields); err != nil {
		return ""
	}
	for _, key := range []string{"version", "versionNonce", "updated", "customData"} {
		delete(fields, key)
	}
	canonical, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:16])
}

// boardStepsWithAfter fills in what each step left behind: for an element a
// later step in the batch also changed, that step's before; otherwise what the
// batch itself writes.
func boardStepsWithAfter(steps [][]db.CollabBoardStepElement, elems []db.CollabBoardElement) [][]db.CollabBoardStepElement {
	after := make(map[string]string, len(elems))
	for _, e := range elems {
		after[e.ID] = boardFingerprint(e.Body)
	}
	for i := len(steps) - 1; i >= 0; i-- {
		for j := range steps[i] {
			each := &steps[i][j]
			each.After = after[each.ID]
			after[each.ID] = boardFingerprint(each.Before)
		}
	}
	return steps
}

// boardRestore builds the write that puts a step element back for actor. An
// element that no longer looks as the step left it has been changed by someone
// else, so it is left alone. The write is a version above both what is stored
// and what comes back, so the merge takes it, and it passes the same checks as
// any edit.
func boardRestore(actor string) db.CollabBoardRestore {
	return func(step db.CollabBoardStepElement, cur *db.CollabBoardElement) (*db.CollabBoardElement, string) {
		if cur != nil && step.After != "" && boardFingerprint(cur.Body) != step.After {
			return nil, ""
		}
		source := step.Before
		deleting := len(source) == 0 || string(source) == "null"
		if deleting {
			source = cur.Body
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(source, &fields); err != nil {
			return nil, ""
		}
		var was struct {
			Version int64 `json:"version"`
		}
		if json.Unmarshal(source, &was) != nil {
			return nil, ""
		}
		version := was.Version
		if cur != nil && cur.Version > version {
			version = cur.Version
		}
		nonce, err := rand.Int(rand.Reader, big.NewInt(1<<31-1))
		if err != nil {
			return nil, ""
		}
		set := func(key string, value any) bool {
			raw, err := json.Marshal(value)
			fields[key] = raw
			return err == nil
		}
		ok := set("id", step.ID) && set("version", version+1) && set("versionNonce", nonce.Int64()+1)
		if ok && deleting {
			ok = set("isDeleted", true)
		}
		raw, err := json.Marshal(fields)
		if !ok || err != nil {
			return nil, ""
		}
		write, err := normalizeBoardElement(raw, actor)
		if err != nil {
			return nil, ""
		}
		return &write, boardFingerprint(write.Body)
	}
}

// POST /api/boards/{id}/undo {redo} — undoes (or redoes) the operator's newest
// step on the board, in whichever editor it was made. skipped counts elements
// left alone because someone else changed them since.
func handleDashboardBoardUndo(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		Redo bool `json:"redo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid undo: "+err.Error())
		return
	}
	result, err := db.UndoCollabBoard(id, boardAuthorOperator, req.Redo, boardRestore(boardAuthorOperator),
		boardMaxLiveElements, boardMaxStoredRows)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	elements := make([]json.RawMessage, 0, len(result.Elements))
	for _, e := range result.Elements {
		elements = append(elements, boardElementJSON(e))
	}
	if len(elements) > 0 {
		boardLive.publish(id, boardClient(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": result.Version, "elements": elements, "skipped": result.Skipped, "empty": result.Empty,
	})
}
