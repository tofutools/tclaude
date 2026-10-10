package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Board version updates (tcl-1gpqb1): an item this node keeps (pins) has a
// newer version on its board. The dashboard badges Fleet → Boards with the
// count; when the operator opts in, the daemon also leaves one Messages note
// per new version. Nothing is fetched or imported: the check reads the
// catalog and pins only.

// boardUpdate is one kept item whose board has a newer version.
type boardUpdate struct {
	Board     string `json:"board"`
	BoardName string `json:"board_name"`
	Item      string `json:"item"`
	Name      string `json:"name"`
	Pinned    string `json:"pinned_version"`
	Latest    string `json:"latest_version"`
}

const (
	boardUpdateInterval = 15 * time.Minute
	boardUpdateFresh    = 5 * time.Minute
	boardUpdateMinGap   = 30 * time.Second
	// Bounds on one scan: boards and catalog pages per board.
	boardUpdateMaxBoardPages = 10
	boardUpdateMaxItemPages  = 20
)

type boardUpdateState struct {
	mu        sync.Mutex
	scanning  sync.Mutex
	checkedAt time.Time
	updates   []boardUpdate
	err       string
	// notified holds board/item/version keys already announced, seeded by
	// the first scan so a daemon restart does not repeat them.
	notified map[string]bool
	seeded   bool
}

var boardUpdates = &boardUpdateState{notified: map[string]bool{}}

// boardUpdateScan is swappable so tests can count scans.
var boardUpdateScan = scanBoardUpdates

// scanBoardUpdates lists every board this node belongs to and reports its
// pinned items whose latest version differs from the pin.
func scanBoardUpdates(ctx context.Context) ([]boardUpdate, error) {
	a, err := newBoardItemAccess(ctx)
	if err != nil {
		return nil, err
	}
	type board struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	boards := []board{}
	cursor := ""
	for page := 0; page < boardUpdateMaxBoardPages; page++ {
		raw, e := a.call("boards.list", map[string]any{"cursor": cursor})
		if e != nil {
			return nil, e
		}
		var reply struct {
			Boards []board `json:"boards"`
			Cursor string  `json:"next_cursor"`
		}
		if e = json.Unmarshal(raw, &reply); e != nil {
			return nil, e
		}
		boards = append(boards, reply.Boards...)
		if cursor = reply.Cursor; cursor == "" {
			break
		}
	}
	out := []boardUpdate{}
	for _, b := range boards {
		pins := map[string]string{}
		cursor = ""
		for {
			raw, e := a.call("pins.list", map[string]any{"board": b.ID, "cursor": cursor})
			if e != nil {
				return nil, e
			}
			var page struct {
				Pins []struct {
					Item    string `json:"item"`
					Version string `json:"version"`
				} `json:"pins"`
				Cursor string `json:"next_cursor"`
			}
			if e = json.Unmarshal(raw, &page); e != nil {
				return nil, e
			}
			for _, p := range page.Pins {
				pins[p.Item] = p.Version
			}
			if cursor = page.Cursor; cursor == "" {
				break
			}
		}
		if len(pins) == 0 {
			continue
		}
		cursor = ""
		for page := 0; page < boardUpdateMaxItemPages; page++ {
			raw, e := a.call("items.list", map[string]any{"board": b.ID, "cursor": cursor})
			if e != nil {
				return nil, e
			}
			var reply struct {
				Items  []proto.BoardItemVersion `json:"items"`
				Cursor string                   `json:"next_cursor"`
			}
			if e = json.Unmarshal(raw, &reply); e != nil {
				return nil, e
			}
			for _, v := range reply.Items {
				pin, kept := pins[v.Item]
				if !kept || pin == v.Version {
					continue
				}
				name := "an item"
				if m, e := a.manifest(v); e == nil && m.Name != "" {
					name = m.Name
				}
				out = append(out, boardUpdate{Board: b.ID, BoardName: b.Name, Item: v.Item, Name: name, Pinned: pin, Latest: v.Version})
			}
			if cursor = reply.Cursor; cursor == "" {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BoardName != out[j].BoardName {
			return out[i].BoardName < out[j].BoardName
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// refresh scans unless a scan finished within minAge, and returns the
// current state. Concurrent callers share one scan.
func (s *boardUpdateState) refresh(ctx context.Context, minAge time.Duration) ([]boardUpdate, time.Time, string) {
	s.scanning.Lock()
	defer s.scanning.Unlock()
	s.mu.Lock()
	fresh := !s.checkedAt.IsZero() && time.Since(s.checkedAt) < minAge
	s.mu.Unlock()
	if !fresh {
		updates, err := boardUpdateScan(ctx)
		s.mu.Lock()
		s.checkedAt = time.Now()
		if err != nil {
			s.err = err.Error()
		} else {
			s.err = ""
			s.updates = updates
		}
		s.mu.Unlock()
		if err == nil {
			s.announce(updates)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]boardUpdate(nil), s.updates...), s.checkedAt, s.err
}

// announce leaves one Messages note per newly seen version when the
// operator opted in. The first scan after start only seeds what is known.
func (s *boardUpdateState) announce(updates []boardUpdate) {
	s.mu.Lock()
	fresh := []boardUpdate{}
	for _, u := range updates {
		k := u.Board + "/" + u.Item + "/" + u.Latest
		if !s.notified[k] {
			s.notified[k] = true
			if s.seeded {
				fresh = append(fresh, u)
			}
		}
	}
	s.seeded = true
	s.mu.Unlock()
	if len(fresh) == 0 || !boardUpdateNotifyEnabled() {
		return
	}
	for _, u := range fresh {
		body := fmt.Sprintf("A newer version of %q was posted on board %q. You keep version %s.\n\nOpen Fleet → Boards to inspect it; nothing is fetched or imported until you do.", u.Name, u.BoardName, shortBoardVersion(u.Pinned))
		if _, err := recordHumanMessage("", "Board update: "+u.Name, body); err != nil {
			slog.Warn("boards: update note", "err", err)
		}
	}
}

func shortBoardVersion(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

func boardUpdateNotifyEnabled() bool {
	cfg, err := config.Load()
	return err == nil && cfg.Federation != nil && cfg.Federation.BoardUpdateNotify
}

func boardHubConfigured() bool {
	cfg, err := config.Load()
	return err == nil && cfg.Federation != nil && cfg.Federation.HubURL != ""
}

// startBoardUpdateChecker scans in the background only while the operator
// wants Messages notes; the dashboard badge otherwise scans on demand.
func startBoardUpdateChecker(stop <-chan struct{}) {
	go func() {
		first := time.NewTimer(2 * time.Minute)
		defer first.Stop()
		ticker := time.NewTicker(boardUpdateInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-first.C:
			case <-ticker.C:
			}
			if !boardHubConfigured() || !boardUpdateNotifyEnabled() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			boardUpdates.refresh(ctx, boardUpdateMinGap)
			cancel()
		}
	}()
}

func registerBoardUpdateRoutes(mux *http.ServeMux, prefix string, dashboard bool) {
	for method, handler := range map[string]http.HandlerFunc{"GET": handleBoardUpdates, "PUT": handleBoardUpdateNotify} {
		h := handler
		if dashboard {
			h = dashboardFederationRoute(h)
		}
		mux.HandleFunc(method+" "+prefix+"/updates", h)
	}
}

// handleBoardUpdates returns the kept items with newer versions. A scan
// runs when the last one is older than five minutes, or on ?refresh=1
// (at most every 30 seconds).
func handleBoardUpdates(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read content board updates") {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	if !boardHubConfigured() {
		writeJSON(w, 200, map[string]any{"updates": []boardUpdate{}, "notify": boardUpdateNotifyEnabled()})
		return
	}
	minAge := boardUpdateFresh
	if r.URL.Query().Get("refresh") == "1" {
		minAge = boardUpdateMinGap
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	updates, at, errText := boardUpdates.refresh(ctx, minAge)
	out := map[string]any{"updates": updates, "notify": boardUpdateNotifyEnabled()}
	if !at.IsZero() {
		out["checked_at"] = at.UTC().Format(time.RFC3339)
	}
	if errText != "" {
		out["error"] = errText
	}
	writeJSON(w, 200, out)
}

// handleBoardUpdateNotify turns the Messages notes on or off: {"notify":bool}.
func handleBoardUpdateNotify(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "configure content board update notes") {
		return
	}
	var in struct {
		Notify *bool `json:"notify"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	if err := dec.Decode(&in); err != nil || in.Notify == nil {
		writeError(w, 400, "invalid_arg", `expected {"notify":true|false}`)
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeError(w, 400, "invalid_arg", "expected one JSON object")
		return
	}
	if _, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Federation == nil {
			cfg.Federation = &config.FederationConfig{}
		}
		cfg.Federation.BoardUpdateNotify = *in.Notify
		return nil
	}); err != nil {
		writeError(w, 500, "config", strings.TrimSpace(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"notify": *in.Notify})
}
