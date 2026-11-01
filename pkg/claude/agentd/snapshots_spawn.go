package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// maxSnapshotSpawn bounds --count.
const maxSnapshotSpawn = 20

type snapshotSpawnRequest struct {
	Group string `json:"group"`
	// Name is the new agents' name; with a count above one each gets a
	// -N suffix. Defaults to the snapshotted agent's name.
	Name      string   `json:"name,omitempty"`
	Count     int      `json:"count,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Worktree  string   `json:"worktree,omitempty"`
	KeepPaths bool     `json:"keep_paths,omitempty"`
	Harness   string   `json:"harness,omitempty"`
	Model     string   `json:"model,omitempty"`
	Set       []string `json:"set,omitempty"`
	// SkipHistory lands the definition only.
	SkipHistory bool `json:"skip_history,omitempty"`
	// ConfirmHistoryLoss accepts a landing that cuts much of the conversation.
	ConfirmHistoryLoss bool `json:"confirm_history_loss,omitempty"`
}

type snapshotSpawnResult struct {
	Snapshot string                `json:"snapshot"`
	Spawned  []agent.SpawnResponse `json:"spawned"`
	Warnings []string              `json:"warnings,omitempty"`
}

// historyLossFacts is what confirming a landing's history cut accepts. How much
// is cut is known only as each agent lands.
const (
	historyLossFacts         = "This accepts a landing that cuts 25% or more of the conversation, if the new agent's model needs it."
	snapshotHistoryLossFacts = historyLossFacts + " The full history stays in the snapshot and is saved beside the agent."
)

func snapshotHistoryLossApprovalFacts(body []byte) string {
	var req snapshotSpawnRequest
	if !decodeLikeHandler(body, &req) || !req.ConfirmHistoryLoss {
		return ""
	}
	return snapshotHistoryLossFacts
}

// handleSnapshotSpawn instantiates new agents from a snapshot.
//
//	POST /v1/snapshots/{name}/spawn {group, name?, count?, cwd?, worktree?, keep_paths?, harness?, model?, set?, skip_history?}
//
// Each agent is an import of the snapshot's PAF: it lands through the same
// rules as `tclaude paf import --apply` (landing harness and model, history
// translation, path remapping, normal spawn checks) and gets a new identity.
// The caller needs agent.bundle.import, plus access to the snapshot's
// content (the human, or agent.bundle.export on the snapshotted agent).
// The from-snapshot link is recorded in the audit log and the new agent's
// arrival briefing; it confers no authority over the new agent.
func handleSnapshotSpawn(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	meta, path, ok := lookupSnapshot(w, name)
	if !ok {
		return
	}
	r = withApprovalFacts(r, snapshotHistoryLossApprovalFacts)
	if !requireSnapshotAccess(w, r, meta, "spawn from a snapshot") {
		return
	}
	var req snapshotSpawnRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, 400, "body", err.Error())
		return
	}
	if req.Count == 0 {
		req.Count = 1
	}
	if req.Count < 1 || req.Count > maxSnapshotSpawn {
		writeError(w, 400, "count", fmt.Sprintf("count must be 1 to %d", maxSnapshotSpawn))
		return
	}
	if req.Group == "" {
		writeError(w, 400, "group", "group is required: the receiving group, as with agent spawn")
		return
	}
	base := req.Name
	if base == "" {
		base = meta.AgentName
	}
	src, err := os.Open(path)
	if err != nil {
		writeError(w, 500, "snapshot", err.Error())
		return
	}
	b, err := decodeAgentArchive(src)
	_ = src.Close()
	if err != nil {
		writeError(w, 500, "snapshot", err.Error())
		return
	}
	source := b.Manifest.Agent
	_ = b.Close()
	caller, human, _ := authedCaller(w, r)

	res := snapshotSpawnResult{Snapshot: name, Spawned: []agent.SpawnResponse{}}
	for i := 1; i <= req.Count; i++ {
		agentName := base
		if req.Count > 1 {
			agentName = base + "-" + strconv.Itoa(i)
		}
		spawned, status, body := spawnOneFromSnapshot(r, path, req, agentName, meta.Name, meta.CreatedAt, source)
		if status != 200 {
			if len(res.Spawned) == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write(body)
				return
			}
			// Earlier agents already exist; report them with the failure.
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(body, &e)
			writeJSON(w, status, map[string]any{"error": fmt.Sprintf("agent %d of %d: %s", i, req.Count, e.Error), "code": "partial", "result": res})
			return
		}
		recordSnapshotSpawn(caller, human, meta.Name, spawned, req.Group)
		res.Spawned = append(res.Spawned, spawned.Spawn)
		if i == 1 {
			res.Warnings = spawned.Warnings
		}
	}
	writeJSON(w, 200, res)
}

type snapshotSpawned struct {
	Spawn    agent.SpawnResponse `json:"spawn"`
	Warnings []string            `json:"warnings"`
}

// spawnOneFromSnapshot lands one agent through the bundle import path.
func spawnOneFromSnapshot(r *http.Request, path string, req snapshotSpawnRequest, agentName, snapshot string, takenAt time.Time, source agentbundle.Definition) (snapshotSpawned, int, []byte) {
	f, err := os.Open(path)
	if err != nil {
		return snapshotSpawned{}, 500, []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	defer func() { _ = f.Close() }()
	q := url.Values{"apply": {"true"}, "group": {req.Group}, "name": {agentName}}
	for k, v := range map[string]string{"cwd": req.Cwd, "worktree": req.Worktree, "harness": req.Harness, "model": req.Model} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if req.KeepPaths {
		q.Set("keep_paths", "true")
	}
	if req.SkipHistory {
		q.Set("skip_history", "true")
	}
	if req.ConfirmHistoryLoss {
		q.Set("confirm_history_loss", "true")
	}
	q["set"] = req.Set
	arrival := &arrivalContext{Operation: "snapshot spawn", Source: source, Reason: "--cwd or recorded path",
		Note: fmt.Sprintf("Instantiated from snapshot %q taken %s. You are a new agent forked from that point; the original agent continues separately.", snapshot, takenAt.UTC().Format(time.RFC3339))}
	inner := r.Clone(context.WithValue(r.Context(), arrivalContextKey{}, arrival))
	inner.Method = http.MethodPost
	inner.URL = &url.URL{Path: "/v1/agent-bundle/import", RawQuery: q.Encode()}
	inner.Body = f
	if st, err := f.Stat(); err == nil {
		inner.ContentLength = st.Size()
	}
	rec := httptest.NewRecorder()
	handleAgentBundleImport(rec, inner)
	if rec.Code != 200 {
		return snapshotSpawned{}, rec.Code, rec.Body.Bytes()
	}
	var out snapshotSpawned
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return snapshotSpawned{}, 500, []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	return out, 200, nil
}

// recordSnapshotSpawn records the from-snapshot link in the audit log.
func recordSnapshotSpawn(caller string, human bool, snapshot string, s snapshotSpawned, group string) {
	e := db.AuditLogEntry{At: time.Now(), ActorKind: db.AuditActorAgent, ActorConv: caller, Verb: "snapshot.spawn",
		TargetConv: s.Spawn.ConvID, TargetAgent: s.Spawn.AgentID, TargetLabel: s.Spawn.Label, GroupName: group,
		Detail: "from_snapshot " + snapshot, Method: http.MethodPost, Path: "/v1/snapshots/" + snapshot + "/spawn", Status: 200, Source: db.AuditSourceCLI}
	if human {
		e.ActorKind, e.ActorConv = db.AuditActorHuman, ""
	} else {
		e.ActorAgent, _ = db.AgentIDForConv(caller)
	}
	if _, err := db.InsertAuditLog(e); err != nil {
		slog.Warn("snapshot: audit write failed", "error", err)
	}
}
