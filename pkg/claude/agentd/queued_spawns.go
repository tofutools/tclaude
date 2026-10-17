package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Queued spawns are spawn-dialog drafts the operator prepared but chose not to
// launch yet. The dashboard owns the draft's shape: the daemon stores it
// opaquely and reads only the target group. Launching one reopens the dialog
// from the draft and submits it through the ordinary spawn path, so a queued
// spawn is validated (permissions, sandbox, worktree) when it launches, not
// when it is queued.

// queuedSpawnMaxBytes caps one stored draft. The initial message alone may be
// 16 KiB; the rest of the form is small.
const queuedSpawnMaxBytes = 256 << 10

type queuedSpawnJSON struct {
	ID        int64           `json:"id"`
	Group     string          `json:"group"`
	Draft     json.RawMessage `json:"draft"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func queuedSpawnWire(q db.QueuedSpawn) queuedSpawnJSON {
	return queuedSpawnJSON{ID: q.ID, Group: q.Group, Draft: json.RawMessage(q.Draft), CreatedAt: q.CreatedAt, UpdatedAt: q.UpdatedAt}
}

// collectQueuedSpawnsSnapshot is the snapshot's queue. Empty slice (not nil)
// so JS .map() is safe; a read error degrades to an empty queue.
func collectQueuedSpawnsSnapshot() []queuedSpawnJSON {
	rows, err := db.ListQueuedSpawns()
	out := make([]queuedSpawnJSON, 0, len(rows))
	if err != nil {
		return out
	}
	for _, q := range rows {
		out = append(out, queuedSpawnWire(q))
	}
	return out
}

func registerDashboardQueuedSpawnRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/queued-spawns", dashboardRolesRoute(handleQueuedSpawns))
	mux.HandleFunc("/api/queued-spawns/{id}", dashboardRolesRoute(handleQueuedSpawnByID))
}

func handleQueuedSpawns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, collectQueuedSpawnsSnapshot())
	case http.MethodPost:
		group, draft, ok := readQueuedSpawnDraft(w, r)
		if !ok {
			return
		}
		id, err := db.CreateQueuedSpawn(group, draft, time.Now())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeQueuedSpawn(w, http.StatusCreated, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

func handleQueuedSpawnByID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id must be a positive integer")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeQueuedSpawn(w, http.StatusOK, id)
	case http.MethodPut:
		group, draft, ok := readQueuedSpawnDraft(w, r)
		if !ok {
			return
		}
		if err := db.UpdateQueuedSpawn(id, group, draft, time.Now()); err != nil {
			writeQueuedSpawnError(w, err)
			return
		}
		writeQueuedSpawn(w, http.StatusOK, id)
	case http.MethodDelete:
		if err := db.DeleteQueuedSpawn(id); err != nil {
			writeQueuedSpawnError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PUT or DELETE")
	}
}

// readQueuedSpawnDraft decodes {"draft": {...}} and returns the draft's target
// group alongside its compact JSON. The draft must be an object naming a group;
// everything else in it is the dashboard's business.
func readQueuedSpawnDraft(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	var req struct {
		Draft json.RawMessage `json:"draft"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, queuedSpawnMaxBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return "", "", false
	}
	var fields struct {
		Group string `json:"group"`
	}
	if len(req.Draft) == 0 || req.Draft[0] != '{' || json.Unmarshal(req.Draft, &fields) != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "draft must be a JSON object")
		return "", "", false
	}
	group := strings.TrimSpace(fields.Group)
	if group == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "draft.group is required")
		return "", "", false
	}
	return group, string(req.Draft), true
}

func writeQueuedSpawn(w http.ResponseWriter, status int, id int64) {
	q, err := db.GetQueuedSpawn(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if q == nil {
		writeQueuedSpawnError(w, db.ErrQueuedSpawnNotFound)
		return
	}
	writeJSON(w, status, queuedSpawnWire(*q))
}

func writeQueuedSpawnError(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrQueuedSpawnNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such queued spawn")
		return
	}
	writeError(w, http.StatusInternalServerError, "io", err.Error())
}
