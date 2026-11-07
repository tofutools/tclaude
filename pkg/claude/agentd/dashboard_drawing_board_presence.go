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

// Drawing board presence (tcl-rawj2c): who is on a drawing board right now, so the operator
// and agents stay oriented without chat. It is in-memory and short-lived:
//
//   - An agent that read the drawing board recently is "looking"; one whose edit
//     landed in the last few seconds is "drawing". The editor shows both.
//   - The operator's editor reports the part of the drawing board on screen, and an
//     agent read carries it as operator_view (`drawing-board show --view` lists only
//     what the operator is looking at).

const (
	drawingBoardDrawingFor    = 6 * time.Second
	drawingBoardLookingFor    = 30 * time.Second
	drawingBoardOperatorView  = 2 * time.Minute
	drawingBoardPresenceLimit = 64
)

type drawingBoardViewport struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type drawingBoardAgentPresence struct {
	AgentID string    `json:"agent_id"`
	State   string    `json:"state"` // drawing | looking
	At      time.Time `json:"at"`
}

type drawingBoardOperatorViewJSON struct {
	drawingBoardViewport
	AgeSeconds int `json:"age_seconds"`
}

type drawingBoardPresenceJSON struct {
	Agents       []drawingBoardAgentPresence   `json:"agents"`
	OperatorView *drawingBoardOperatorViewJSON `json:"operator_view,omitempty"`
}

type drawingBoardActivity struct{ readAt, editAt time.Time }

type drawingBoardViewAt struct {
	view drawingBoardViewport
	at   time.Time
}

type drawingBoardPresenceEntry struct {
	agents map[string]*drawingBoardActivity
	// views holds one viewport per open editor (a dashboard tab's client
	// token); the newest is the operator's view, so a background tab
	// clearing its own never hides the one in front.
	views map[string]drawingBoardViewAt
}

type drawingBoardPresenceStore struct {
	sync.Mutex
	drawingBoards map[string]*drawingBoardPresenceEntry
}

var drawingBoardPresence = &drawingBoardPresenceStore{drawingBoards: map[string]*drawingBoardPresenceEntry{}}

func (s *drawingBoardPresenceStore) entry(id string) *drawingBoardPresenceEntry {
	e := s.drawingBoards[id]
	if e == nil {
		e = &drawingBoardPresenceEntry{agents: map[string]*drawingBoardActivity{}, views: map[string]drawingBoardViewAt{}}
		s.drawingBoards[id] = e
	}
	return e
}

func (s *drawingBoardPresenceStore) activity(id, agentID string) *drawingBoardActivity {
	e := s.entry(id)
	a := e.agents[agentID]
	if a == nil {
		// Bounded: the stalest agent makes room.
		if len(e.agents) >= drawingBoardPresenceLimit {
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
		a = &drawingBoardActivity{}
		e.agents[agentID] = a
	}
	return a
}

func (s *drawingBoardPresenceStore) noteRead(id, agentID string) {
	if agentID == "" {
		return
	}
	s.Lock()
	defer s.Unlock()
	s.activity(id, agentID).readAt = drawingBoardNow()
}

func (s *drawingBoardPresenceStore) noteEdit(id, agentID string) {
	if agentID == "" {
		return
	}
	s.Lock()
	defer s.Unlock()
	a := s.activity(id, agentID)
	a.editAt = drawingBoardNow()
	a.readAt = a.editAt
}

const drawingBoardMaxViewClients = 16

func (s *drawingBoardPresenceStore) setView(id, client string, view *drawingBoardViewport) {
	s.Lock()
	defer s.Unlock()
	e := s.entry(id)
	if view == nil {
		delete(e.views, client)
		return
	}
	now := drawingBoardNow()
	for k, v := range e.views {
		if now.Sub(v.at) >= drawingBoardOperatorView {
			delete(e.views, k)
		}
	}
	if _, ok := e.views[client]; !ok && len(e.views) >= drawingBoardMaxViewClients {
		return
	}
	e.views[client] = drawingBoardViewAt{view: *view, at: now}
}

func (s *drawingBoardPresenceStore) forget(id string) {
	s.Lock()
	defer s.Unlock()
	delete(s.drawingBoards, id)
}

func (s *drawingBoardPresenceStore) snapshot(id string) *drawingBoardPresenceJSON {
	s.Lock()
	defer s.Unlock()
	out := &drawingBoardPresenceJSON{Agents: []drawingBoardAgentPresence{}}
	e := s.drawingBoards[id]
	if e == nil {
		return out
	}
	now := drawingBoardNow()
	for agentID, a := range e.agents {
		switch {
		case now.Sub(a.editAt) < drawingBoardDrawingFor:
			out.Agents = append(out.Agents, drawingBoardAgentPresence{AgentID: agentID, State: "drawing", At: a.editAt})
		case now.Sub(a.readAt) < drawingBoardLookingFor:
			out.Agents = append(out.Agents, drawingBoardAgentPresence{AgentID: agentID, State: "looking", At: a.readAt})
		}
	}
	slices.SortFunc(out.Agents, func(a, b drawingBoardAgentPresence) int { return strings.Compare(a.AgentID, b.AgentID) })
	var newest *drawingBoardViewAt
	for _, v := range e.views {
		if now.Sub(v.at) < drawingBoardOperatorView && (newest == nil || v.at.After(newest.at)) {
			v := v
			newest = &v
		}
	}
	if newest != nil {
		out.OperatorView = &drawingBoardOperatorViewJSON{drawingBoardViewport: newest.view, AgeSeconds: int(now.Sub(newest.at).Seconds())}
	}
	return out
}

// POST /api/drawing-boards/{id}/view {client, x, y, width, height} — the part of the
// drawing board on screen in one editor (client is that tab's token); without the
// coordinates it clears that editor's view (the operator looked away).
func handleDashboardDrawingBoardView(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		Client              string
		X, Y, Width, Height *float64
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid view: "+err.Error())
		return
	}
	if len(req.Client) > 64 || !drawingBoardPrintable(req.Client, false) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "client must be a short printable token")
		return
	}
	// Only real drawing boards get presence state.
	if _, err := db.GetCollabDrawingBoard(id); err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	if req.X == nil && req.Y == nil && req.Width == nil && req.Height == nil {
		drawingBoardPresence.setView(id, req.Client, nil)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	finite := func(v *float64, lo, hi float64) bool { return v != nil && !math.IsNaN(*v) && *v >= lo && *v <= hi }
	if !finite(req.X, -drawingBoardMaxCoordinate*2, drawingBoardMaxCoordinate*2) || !finite(req.Y, -drawingBoardMaxCoordinate*2, drawingBoardMaxCoordinate*2) ||
		!finite(req.Width, 1, drawingBoardMaxCoordinate*4) || !finite(req.Height, 1, drawingBoardMaxCoordinate*4) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "x, y, width and height must be finite and in range (width and height at least 1)")
		return
	}
	drawingBoardPresence.setView(id, req.Client, &drawingBoardViewport{X: *req.X, Y: *req.Y, Width: *req.Width, Height: *req.Height})
	w.WriteHeader(http.StatusNoContent)
}
