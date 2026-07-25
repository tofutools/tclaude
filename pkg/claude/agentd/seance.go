package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	maxSeanceRequestBytes = 4 << 10
	maxSeanceTargetBytes  = 256
	maxSeanceBack         = 128
)

// seanceResolveReq is the non-billable planning request from the sandboxed
// agent CLI. Agent selectors identify an actor and walk back from its live
// head; exact conv-id selectors identify one historical generation.
type seanceResolveReq struct {
	Target string `json:"target"`
	Back   int    `json:"back"`
}

type seanceResolveResp struct {
	Predecessor string `json:"predecessor"`
	Harness     string `json:"harness"`
	Cwd         string `json:"cwd"`
	Hops        int    `json:"hops"`
	Requested   int    `json:"requested_back"`
	Exact       bool   `json:"exact"`
}

// handleWhoamiSeance resolves the private-state half of a séance plan.
//
// The actual harness subprocess remains in the calling CLI: agentd owns the
// succession/session/index reads under ~/.tclaude/data, while the caller keeps
// the billable execution inside its existing sandbox and approval posture.
//
// Agents may consult only generations of their own stable actor. The human
// operator may target any actor/generation. Cross-agent séance can grow an
// explicit permission-gated endpoint later without silently granting that
// capability as part of this self-scoped repair.
func handleWhoamiSeance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}

	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	var req seanceResolveReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSeanceRequestBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body")
		return
	}
	req.Target = strings.TrimSpace(req.Target)
	if len(req.Target) > maxSeanceTargetBytes {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("--target is too long (maximum %d bytes)", maxSeanceTargetBytes))
		return
	}
	if req.Back < 1 {
		req.Back = 1
	}
	if req.Back > maxSeanceBack {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("--back must be between 1 and %d", maxSeanceBack))
		return
	}

	target, hops, exact, ok := resolveSeanceGeneration(w, req, caller, isHuman)
	if !ok {
		return
	}
	if !isHuman && !sameActor(caller, target) {
		writeError(w, http.StatusForbidden, "permission",
			"an agent may hold a séance only with one of its own predecessor generations")
		return
	}

	loc := agent.ResolveLocation(target)
	cwd := strings.TrimSpace(loc.StartupDir)
	if cwd == "" {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("cannot locate predecessor %s's working directory; its grave is unreachable", short8(target)))
		return
	}
	info, err := os.Stat(cwd)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("predecessor %s's working directory no longer exists; its grave is unreachable: %s",
					short8(target), cwd))
			return
		}
		writeError(w, http.StatusConflict, "unreachable_grave",
			fmt.Sprintf("cannot access predecessor %s's working directory; its grave is unreachable: %v",
				short8(target), err))
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusConflict, "unreachable_grave",
			fmt.Sprintf("predecessor %s's recorded working directory is not a directory; its grave is unreachable: %s",
				short8(target), cwd))
		return
	}

	harnessName := ""
	if row := agent.FreshConvRowResolved(target); row != nil {
		harnessName = row.Harness
	}
	if harnessName == "" {
		if sess, serr := db.FindSessionByConvID(target); serr == nil && sess != nil {
			harnessName = sess.Harness
		}
	}
	h, err := harness.Resolve(harnessName)
	if err != nil {
		writeError(w, http.StatusConflict, "unsupported_harness", err.Error())
		return
	}
	if !h.SupportsAsk() {
		writeError(w, http.StatusConflict, "unsupported_harness",
			fmt.Sprintf("harness %q cannot hold a séance (no headless resume/ask support)", h.Name))
		return
	}

	writeJSON(w, http.StatusOK, seanceResolveResp{
		Predecessor: target,
		Harness:     h.Name,
		Cwd:         cwd,
		Hops:        hops,
		Requested:   req.Back,
		Exact:       exact,
	})
}

func resolveSeanceGeneration(
	w http.ResponseWriter,
	req seanceResolveReq,
	caller string,
	isHuman bool,
) (target string, hops int, exact bool, ok bool) {
	if req.Target == "" {
		if isHuman {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"the human operator must pass --target; there is no calling agent predecessor")
			return "", 0, false, false
		}
		target, hops, ok = walkSeancePredecessor(w, caller, req.Back)
		return target, hops, false, ok
	}

	// Conversation IDs are generation selectors, deliberately resolved before
	// the general agent selector so a predecessor never redirects to its live
	// head. The durable lookup includes succession rows, so a historical
	// generation stays exact even after its conv_index cache row is pruned.
	exactID, matchCount, shortPrefix, err := exactSeanceGeneration(req.Target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return "", 0, false, false
	}
	if shortPrefix {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("conversation prefix %q is too short; pass at least 8 characters", req.Target))
		return "", 0, false, false
	}
	if matchCount > 1 {
		writeError(w, http.StatusConflict, "ambiguous",
			fmt.Sprintf("conversation prefix %q matches multiple generations; pass at least 8 unique characters", req.Target))
		return "", 0, false, false
	}
	if exactID != "" {
		successor, serr := db.GetConvSuccessor(exactID)
		if serr != nil {
			writeError(w, http.StatusInternalServerError, "io", serr.Error())
			return "", 0, false, false
		}
		if successor == "" {
			writeError(w, http.StatusConflict, "invalid_arg",
				fmt.Sprintf("conversation %s is a live generation or was never superseded; a séance requires a dead predecessor",
					short8(exactID)))
			return "", 0, false, false
		}
		return exactID, 0, true, true
	}

	res, matches, rerr := agent.ResolveSelectorCached(req.Target)
	if errors.Is(rerr, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "selector matches multiple agents",
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return "", 0, false, false
	}
	if rerr != nil {
		writeError(w, http.StatusNotFound, "not_found", rerr.Error())
		return "", 0, false, false
	}
	if res.AgentID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("selector %q resolves to a conversation, not an agent", req.Target))
		return "", 0, false, false
	}
	if !isHuman && !sameActor(caller, res.ConvID) {
		writeError(w, http.StatusForbidden, "permission",
			"an agent may hold a séance only with one of its own predecessor generations")
		return "", 0, false, false
	}
	target, hops, ok = walkSeancePredecessor(w, res.ConvID, req.Back)
	return target, hops, false, ok
}

// exactSeanceGeneration returns (id, matchCount, shortPrefix, error). A
// matchCount greater than one is an ambiguous prefix; zero means the selector
// should fall through to stable agent-id/name resolution. Conversation IDs are
// deliberately not assumed to be UUIDs: OpenCode uses ses_... IDs.
func exactSeanceGeneration(selector string) (string, int, bool, error) {
	if strings.HasPrefix(selector, db.AgentIDPrefix) {
		return "", 0, false, nil
	}
	ids, err := db.FindKnownConvIDsByPrefix(selector, 2)
	if err != nil {
		return "", 0, false, err
	}
	if len(selector) < 8 && len(ids) > 0 {
		return "", len(ids), true, nil
	}
	if len(ids) == 1 {
		return ids[0], 1, false, nil
	}
	return "", len(ids), false, nil
}

func walkSeancePredecessor(w http.ResponseWriter, head string, back int) (string, int, bool) {
	target, hops, err := db.ResolvePredecessorN(head, back)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return "", 0, false
	}
	if target == "" {
		writeError(w, http.StatusNotFound, "not_found",
			"you have no predecessor to consult — this conversation was not reincarnated from another agent")
		return "", 0, false
	}
	return target, hops, true
}
