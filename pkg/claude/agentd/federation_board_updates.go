package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	// Bounds on one scan. The hub pages catalogs 8 items at a time in item
	// order; paging stops past the last kept item, so the cap only bounds a
	// pathological board (1,000 versions by default fit in 125 pages).
	boardUpdateMaxBoardPages = 10
	boardUpdateMaxPinPages   = 50
	boardUpdateMaxItemPages  = 150
	boardUpdateScanTimeout   = 2 * time.Minute
	// Announced versions are remembered this long (the notes file stays small).
	boardUpdateNoteRetention = 180 * 24 * time.Hour
)

type boardUpdateState struct {
	mu        sync.Mutex
	scanning  sync.Mutex
	checkedAt time.Time
	updates   []boardUpdate
	err       string
	// notified holds board/item/version keys already announced (with when),
	// persisted so versions posted while the daemon was down are announced
	// once it is back, and a restart repeats nothing. Only the very first
	// scan on a node (no notes file yet) seeds silently.
	notified map[string]time.Time
	loaded   bool
	seeded   bool
}

var boardUpdates = &boardUpdateState{}

// boardUpdateNotesPath is swappable so tests keep their notes private.
var boardUpdateNotesPath = func() string { return filepath.Join(config.DataDir(), "board-update-notes.json") }

func (s *boardUpdateState) loadNotes() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.notified = map[string]time.Time{}
	raw, err := os.ReadFile(boardUpdateNotesPath())
	if err != nil {
		return
	}
	if json.Unmarshal(raw, &s.notified) == nil {
		s.seeded = true
	}
	if s.notified == nil {
		s.notified = map[string]time.Time{}
	}
}

func (s *boardUpdateState) saveNotes() {
	for k, at := range s.notified {
		if time.Since(at) > boardUpdateNoteRetention {
			delete(s.notified, k)
		}
	}
	raw, err := json.Marshal(s.notified)
	if err == nil {
		err = writeBoardNotes(boardUpdateNotesPath(), raw)
	}
	if err != nil {
		slog.Warn("boards: saving update notes", "err", err)
	}
}

func writeBoardNotes(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// expire makes the next read check again (after a pin, leave or delete).
func (s *boardUpdateState) expire() {
	s.mu.Lock()
	s.checkedAt = time.Time{}
	s.mu.Unlock()
}

// boardUpdateScan is swappable so tests can count scans.
var boardUpdateScan = scanBoardUpdates

// scanBoardUpdates lists every board this node belongs to and reports its
// pinned items whose latest version differs from the pin.
func scanBoardUpdates(ctx context.Context, previous []boardUpdate) ([]boardUpdate, error) {
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
	failed := []string{}
	for _, b := range boards {
		found, e := scanBoardUpdatesOn(a, b.ID, b.Name)
		if e != nil {
			// One unreadable board keeps what it showed before and does not
			// hide the others.
			failed = append(failed, fmt.Sprintf("%s: %v", b.Name, e))
			for _, u := range previous {
				if u.Board == b.ID {
					out = append(out, u)
				}
			}
			continue
		}
		out = append(out, found...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BoardName != out[j].BoardName {
			return out[i].BoardName < out[j].BoardName
		}
		return out[i].Name < out[j].Name
	})
	if len(failed) > 0 {
		return out, fmt.Errorf("could not check %s", strings.Join(failed, "; "))
	}
	return out, nil
}

func scanBoardUpdatesOn(a *boardItemAccess, boardID, boardName string) ([]boardUpdate, error) {
	b := struct{ ID, Name string }{boardID, boardName}
	out := []boardUpdate{}
	{
		pins := map[string]string{}
		cursor := ""
		for page := 0; ; page++ {
			if page == boardUpdateMaxPinPages {
				return nil, errors.New("too many kept items")
			}
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
			return out, nil
		}
		last := ""
		for item := range pins {
			if item > last {
				last = item
			}
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
			// The catalog is in item order: past the last kept item there
			// is nothing left to compare.
			if cursor = reply.Cursor; cursor == "" || cursor >= last {
				break
			}
		}
	}
	return out, nil
}

// refresh scans unless a scan finished within minAge, and returns the
// current state. Concurrent callers share one scan.
func (s *boardUpdateState) refresh(ctx context.Context, minAge time.Duration) ([]boardUpdate, time.Time, string) {
	s.scanning.Lock()
	defer s.scanning.Unlock()
	s.mu.Lock()
	age := time.Since(s.checkedAt)
	// A failed check is retried after the minimum gap, not the fresh window.
	fresh := !s.checkedAt.IsZero() && (age < boardUpdateMinGap || (s.err == "" && age < minAge))
	previous := append([]boardUpdate(nil), s.updates...)
	s.mu.Unlock()
	if !fresh {
		// The scan outlives the request that started it: a client giving up
		// must not turn into a cached failure for everyone else.
		scanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boardUpdateScanTimeout)
		updates, err := boardUpdateScan(scanCtx, previous)
		cancel()
		s.mu.Lock()
		s.checkedAt = time.Now()
		if err != nil {
			s.err = err.Error()
		} else {
			s.err = ""
		}
		if updates != nil {
			s.updates = updates
		}
		s.mu.Unlock()
		if updates != nil {
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
	s.loadNotes()
	fresh := []boardUpdate{}
	changed := !s.seeded
	for _, u := range updates {
		k := u.Board + "/" + u.Item + "/" + u.Latest
		if _, done := s.notified[k]; !done {
			s.notified[k] = time.Now()
			changed = true
			if s.seeded {
				fresh = append(fresh, u)
			}
		}
	}
	s.seeded = true
	if changed {
		s.saveNotes()
	}
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
			boardUpdates.refresh(context.Background(), boardUpdateMinGap)
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
	updates, at, errText := boardUpdates.refresh(r.Context(), minAge)
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
