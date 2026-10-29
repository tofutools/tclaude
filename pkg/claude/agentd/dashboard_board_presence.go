package agentd

import (
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Board presence (tcl-rawj2c): who is on a board right now, so the operator
// and agents stay oriented without chat. It is in-memory and short-lived:
//
//   - An agent that read the board recently is "looking"; one whose edit
//     landed in the last few seconds is "drawing". The editor shows both.
//   - The operator's editor reports the part of the board on screen, and an
//     agent read carries it as operator_view (`board show --view` lists only
//     what the operator is looking at).

const (
	boardDrawingFor    = 6 * time.Second
	boardLookingFor    = 30 * time.Second
	boardOperatorView  = 2 * time.Minute
	boardPresenceLimit = 64
)

type boardViewport struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type boardAgentPresence struct {
	AgentID string    `json:"agent_id"`
	State   string    `json:"state"` // drawing | looking
	At      time.Time `json:"at"`
}

type boardOperatorViewJSON struct {
	boardViewport
	AgeSeconds int `json:"age_seconds"`
}

type boardPresenceJSON struct {
	Agents       []boardAgentPresence   `json:"agents"`
	OperatorView *boardOperatorViewJSON `json:"operator_view,omitempty"`
}

type boardActivity struct{ readAt, editAt time.Time }

type boardPresenceEntry struct {
	agents map[string]*boardActivity
	view   *boardViewport
	viewAt time.Time
}

type boardPresenceStore struct {
	sync.Mutex
	boards map[string]*boardPresenceEntry
}

var boardPresence = &boardPresenceStore{boards: map[string]*boardPresenceEntry{}}

func (s *boardPresenceStore) entry(id string) *boardPresenceEntry {
	e := s.boards[id]
	if e == nil {
		e = &boardPresenceEntry{agents: map[string]*boardActivity{}}
		s.boards[id] = e
	}
	return e
}

func (s *boardPresenceStore) activity(id, agentID string) *boardActivity {
	e := s.entry(id)
	a := e.agents[agentID]
	if a == nil {
		// Bounded: the stalest agent makes room.
		if len(e.agents) >= boardPresenceLimit {
			var oldest string
			var at time.Time
			for k, v := range e.agents {
				last := v.readAt
				if v.editAt.After(last) {
					last = v.editAt
				}
				if oldest == "" || last.Before(at) {
					oldest, at = k, last
				}
			}
			delete(e.agents, oldest)
		}
		a = &boardActivity{}
		e.agents[agentID] = a
	}
	return a
}

func (s *boardPresenceStore) noteRead(id, agentID string) {
	if agentID == "" {
		return
	}
	s.Lock()
	defer s.Unlock()
	s.activity(id, agentID).readAt = boardNow()
}

func (s *boardPresenceStore) noteEdit(id, agentID string) {
	if agentID == "" {
		return
	}
	s.Lock()
	defer s.Unlock()
	a := s.activity(id, agentID)
	a.editAt = boardNow()
	a.readAt = a.editAt
}

func (s *boardPresenceStore) setView(id string, view *boardViewport) {
	s.Lock()
	defer s.Unlock()
	e := s.entry(id)
	e.view, e.viewAt = view, boardNow()
}

func (s *boardPresenceStore) forget(id string) {
	s.Lock()
	defer s.Unlock()
	delete(s.boards, id)
}

func (s *boardPresenceStore) snapshot(id string) *boardPresenceJSON {
	s.Lock()
	defer s.Unlock()
	out := &boardPresenceJSON{Agents: []boardAgentPresence{}}
	e := s.boards[id]
	if e == nil {
		return out
	}
	now := boardNow()
	for agentID, a := range e.agents {
		switch {
		case now.Sub(a.editAt) < boardDrawingFor:
			out.Agents = append(out.Agents, boardAgentPresence{AgentID: agentID, State: "drawing", At: a.editAt})
		case now.Sub(a.readAt) < boardLookingFor:
			out.Agents = append(out.Agents, boardAgentPresence{AgentID: agentID, State: "looking", At: a.readAt})
		}
	}
	slices.SortFunc(out.Agents, func(a, b boardAgentPresence) int { return strings.Compare(a.AgentID, b.AgentID) })
	if e.view != nil && now.Sub(e.viewAt) < boardOperatorView {
		out.OperatorView = &boardOperatorViewJSON{boardViewport: *e.view, AgeSeconds: int(now.Sub(e.viewAt).Seconds())}
	}
	return out
}

// POST /api/boards/{id}/view {x, y, width, height} — the part of the board
// on the operator's screen; {} clears it (the operator left the board).
func handleDashboardBoardView(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		X, Y, Width, Height *float64
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid view: "+err.Error())
		return
	}
	// Only real boards get presence state.
	if _, err := db.GetCollabBoard(id); err != nil {
		writeBoardStoreError(w, err)
		return
	}
	if req.X == nil && req.Y == nil && req.Width == nil && req.Height == nil {
		boardPresence.setView(id, nil)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	finite := func(v *float64, lo, hi float64) bool { return v != nil && !math.IsNaN(*v) && *v >= lo && *v <= hi }
	if !finite(req.X, -boardMaxCoordinate*2, boardMaxCoordinate*2) || !finite(req.Y, -boardMaxCoordinate*2, boardMaxCoordinate*2) ||
		!finite(req.Width, 1, boardMaxCoordinate*4) || !finite(req.Height, 1, boardMaxCoordinate*4) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "x, y, width and height must be finite and in range (width and height at least 1)")
		return
	}
	boardPresence.setView(id, &boardViewport{X: *req.X, Y: *req.Y, Width: *req.Width, Height: *req.Height})
	w.WriteHeader(http.StatusNoContent)
}
