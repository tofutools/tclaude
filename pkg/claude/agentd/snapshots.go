package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/snapshotlib"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/paf"
)

// Bounds on snapshots an agent creates, since only the human removes them.
const (
	agentSnapshotMaxRetention = 30 * 24 * time.Hour
	agentSnapshotLibraryCap   = 200
)

// Snapshot capture states, as agent.json snapshot.state names them.
const (
	snapshotStateIdle             = "idle"
	snapshotStateLastCompleteTurn = "last_complete_turn"
)

var (
	snapshotStoresMu sync.Mutex
	snapshotStores   = map[string]*snapshotlib.Store{}
)

// snapshotStore is the node's snapshot library under the data directory.
// One store per directory, so its lock orders every change to that library.
func snapshotStore() *snapshotlib.Store {
	dir := filepath.Join(config.DataDir(), "snapshots")
	snapshotStoresMu.Lock()
	defer snapshotStoresMu.Unlock()
	s := snapshotStores[dir]
	if s == nil {
		s = snapshotlib.New(dir)
		snapshotStores[dir] = s
	}
	return s
}

type snapshotCreateRequest struct {
	Agent string `json:"agent"`
	Name  string `json:"name,omitempty"`
	Note  string `json:"note,omitempty"`
	// Expires is a duration ("720h", "30d") or an RFC 3339 time; "never"
	// keeps the snapshot until removed. Empty uses snapshots.default_retention.
	Expires string `json:"expires,omitempty"`
	// NoHistory captures the definition only.
	NoHistory bool `json:"no_history,omitempty"`
	// Workflow and OwnWorkflows name workflows the snapshot carries.
	Workflow     []string `json:"workflow,omitempty"`
	OwnWorkflows bool     `json:"own_workflows,omitempty"`
	// Summary is the id of a summary the agent wrote for this capture
	// (POST /v1/agent-bundle/summary); it travels as context/handoff.md.
	Summary string `json:"summary,omitempty"`
}

// handleSnapshotCreate captures a named snapshot of an agent into the local
// library.
//
//	POST /v1/snapshots {agent, name?, note?, expires?, no_history?}
//
// A snapshot is a local PAF with the agent's definition, environment and,
// by default, its conversation (native history plus neutral context). It
// needs agent.bundle.export on the agent, like an export: it holds the same
// content. It never leaves the node, so credential findings are recorded
// rather than refused.
func handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	var req snapshotCreateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, 400, "body", err.Error())
		return
	}
	res, _, err := agent.ResolveSelector(req.Agent)
	if err != nil {
		writeError(w, 404, "agent", err.Error())
		return
	}
	if _, ok := requireCrossAgentPermission(w, r, PermAgentBundleExport, res.ConvID); !ok {
		return
	}
	if len(req.Note) > 1000 {
		writeError(w, 400, "note", "note is longer than 1000 characters")
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	expires, err := snapshotExpiry(req.Expires, now)
	if err != nil {
		writeError(w, 400, "expires", err.Error())
		return
	}
	if _, human, _ := authedCaller(w, r); !human {
		// Only the human removes snapshots, so an agent's are bounded: they
		// expire within agentSnapshotMaxRetention, and an agent cannot grow
		// the library past agentSnapshotLibraryCap.
		limit := now.Add(agentSnapshotMaxRetention)
		if expires == nil || expires.After(limit) {
			expires = &limit
		}
		// Expired snapshots do not count against the cap.
		_, _ = snapshotStore().Prune(now)
		if list, err := snapshotStore().List(); err == nil && len(list) >= agentSnapshotLibraryCap {
			writeError(w, 429, "library_full", fmt.Sprintf("the snapshot library holds %d snapshots; only the human can add more", len(list)))
			return
		}
	}
	// Capture state is read before the history: an idle agent stays a
	// complete capture; a busy one is captured up to its last complete
	// turn, as the harness history export writes only finished records.
	state := snapshotStateLastCompleteTurn
	if row, err := db.FindSessionByConvID(res.ConvID); err == nil && (row == nil || row.Status == session.StatusIdle || row.Status == session.StatusExited) {
		state = snapshotStateIdle
	}
	b, err := collectAgentBundle(res.ConvID, !req.NoHistory)
	if err != nil {
		writeError(w, 400, "snapshot", err.Error())
		return
	}
	defer func() { _ = b.Close() }()
	d := b.Manifest.Agent
	name := req.Name
	if name == "" {
		name = defaultSnapshotName(d.Name, now)
	}
	if !snapshotlib.ValidName(name) {
		writeError(w, 400, "name", fmt.Sprintf("invalid snapshot name %q: use 1-64 of a-z, 0-9, '.', '_' and '-', starting with a letter or digit", name))
		return
	}
	store := snapshotStore()
	// Expired snapshots go first, so an expired name is free again.
	_, _ = store.Prune(now)
	from := &paf.Ref{Conversation: res.ConvID}
	meta := snapshotlib.Meta{Name: name, Note: req.Note, AgentName: d.Name, ConvID: res.ConvID, Harness: d.Harness, State: state, CreatedAt: now, ExpiresAt: expires, History: b.Manifest.History != nil}
	if o := d.Origin; o != nil {
		from.Instance, from.Agent = o.Instance, o.Agent
		meta.AgentID, meta.Model = o.Agent, o.Model
	}
	for _, f := range b.Manifest.Findings {
		meta.Findings = append(meta.Findings, paf.Finding{Kind: f.Kind, Count: f.Count, Locations: f.Locations})
	}
	var handoff string
	if req.Summary != "" {
		caller, _, _ := authedCaller(w, r)
		if handoff, err = takeExportSummary(req.Summary, caller, res.ConvID); err != nil {
			writeError(w, 409, "summary", err.Error())
			return
		}
	}
	parts := pafHistoryParts{
		Handoff:  handoff,
		Snapshot: &paf.Snapshot{Name: name, Note: req.Note, TakenAt: now, State: state},
		Lineage:  []paf.LineageStep{{Op: paf.OpSnapshot, At: now, From: from, Detail: "snapshot " + name}},
		// A snapshot can be shared on a board, and its signature rules out
		// rewriting it then.
		LossCountsOnly: true,
	}
	workflows, ok := loadCarriedWorkflows(w, r, req.carried(), agentOwnWorkflows(res.ConvID))
	if !ok {
		return
	}
	parts.Workflows = workflows
	carriedFindings := scanWorkflowBundles(workflows)
	meta.Findings = append(meta.Findings, carriedFindings...)
	b.Manifest.Findings = append(b.Manifest.Findings, bundleFindings(carriedFindings)...)
	archive, err := archiveAgentPAFWith(b, false, parts)
	if err != nil {
		writeError(w, 400, "snapshot", err.Error())
		return
	}
	if req.Summary != "" {
		dropExportSummary(req.Summary)
	}
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	meta, err = store.Create(meta, func(w io.Writer) error { _, err := io.Copy(w, archive); return err })
	switch {
	case errors.Is(err, snapshotlib.ErrExists):
		writeError(w, 409, "exists", fmt.Sprintf("snapshot %q already exists", name))
		return
	case err != nil:
		writeError(w, 500, "snapshot", err.Error())
		return
	}
	writeJSON(w, 200, meta)
}

func (req snapshotCreateRequest) carried() carriedWorkflows {
	return carriedWorkflows{IDs: req.Workflow, Own: req.OwnWorkflows}
}

var nonNameChars = regexp.MustCompile(`[^a-z0-9._-]+`)

// defaultSnapshotName is <agent>-<UTC timestamp>, reduced to the name
// charset.
func defaultSnapshotName(agentName string, at time.Time) string {
	base := strings.Trim(nonNameChars.ReplaceAllString(strings.ToLower(agentName), "-"), "-._")
	if base == "" {
		base = "agent"
	}
	if len(base) > 40 {
		base = strings.TrimRight(base[:40], "-._")
	}
	return base + "-" + at.UTC().Format("20060102-150405")
}

// snapshotExpiry parses an expiry: a duration (Go syntax, or N followed by
// d for days), an RFC 3339 time, "never", or empty for the configured
// default.
func snapshotExpiry(v string, now time.Time) (*time.Time, error) {
	if v == "" {
		if cfg, err := config.Load(); err == nil && cfg.Snapshots != nil {
			v = cfg.Snapshots.DefaultRetention
		}
		if v == "" {
			return nil, nil
		}
		t, err := snapshotExpiry(v, now)
		if err != nil {
			return nil, fmt.Errorf("config snapshots.default_retention: %w", err)
		}
		return t, nil
	}
	if v == "never" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		if !t.After(now) {
			return nil, errors.New("expiry is in the past")
		}
		t = t.UTC()
		return &t, nil
	}
	d, err := parseRetention(v)
	if err != nil {
		return nil, fmt.Errorf("expiry %q: use a duration such as 72h or 30d, an RFC 3339 time, or never", v)
	}
	t := now.Add(d)
	return &t, nil
}

func parseRetention(v string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > 36500 {
			return 0, errors.New("bad day count")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, errors.New("bad duration")
	}
	return d, nil
}

// handleSnapshotList lists the library, newest first, after pruning
// expired snapshots.
//
//	GET /v1/snapshots
func handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	_, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	store := snapshotStore()
	_, _ = store.Prune(time.Now())
	list, err := store.List()
	if err != nil {
		writeError(w, 500, "snapshots", err.Error())
		return
	}
	if !human {
		for i := range list {
			list[i] = agentSnapshotView(list[i])
		}
	}
	writeJSON(w, 200, map[string]any{"snapshots": list})
}

// handleSnapshotShow returns one snapshot's metadata.
//
//	GET /v1/snapshots/{name}
func handleSnapshotShow(w http.ResponseWriter, r *http.Request) {
	_, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	meta, _, ok := lookupSnapshot(w, r.PathValue("name"))
	if !ok {
		return
	}
	if !human {
		meta = agentSnapshotView(meta)
	}
	writeJSON(w, 200, meta)
}

// agentSnapshotView is what an agent sees of a snapshot it may not be able
// to export: the free-text note and where in the history credentials were
// found are left out.
func agentSnapshotView(m snapshotlib.Meta) snapshotlib.Meta {
	m.Note = ""
	findings := make([]paf.Finding, 0, len(m.Findings))
	for _, f := range m.Findings {
		findings = append(findings, paf.Finding{Kind: f.Kind, Count: f.Count})
	}
	m.Findings = findings
	return m
}

func lookupSnapshot(w http.ResponseWriter, name string) (snapshotlib.Meta, string, bool) {
	if !snapshotlib.ValidName(name) {
		writeError(w, 400, "name", fmt.Sprintf("invalid snapshot name %q", name))
		return snapshotlib.Meta{}, "", false
	}
	meta, path, err := snapshotStore().Get(name)
	switch {
	case errors.Is(err, snapshotlib.ErrNotFound):
		writeError(w, 404, "not_found", fmt.Sprintf("no snapshot %q", name))
		return snapshotlib.Meta{}, "", false
	case err != nil:
		writeError(w, 500, "snapshot", err.Error())
		return snapshotlib.Meta{}, "", false
	}
	if meta.Expired(time.Now()) {
		writeError(w, 404, "expired", fmt.Sprintf("snapshot %q has expired", name))
		return snapshotlib.Meta{}, "", false
	}
	return meta, path, true
}

// handleSnapshotExport streams a snapshot's PAF. It holds the agent's
// history, so it needs what an export of the agent needs; a human always
// may.
//
//	GET /v1/snapshots/{name}/export
func handleSnapshotExport(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := authedCaller(w, r); !ok {
		return
	}
	meta, path, ok := lookupSnapshot(w, r.PathValue("name"))
	if !ok {
		return
	}
	if !requireSnapshotAccess(w, r, meta, "export a snapshot") {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, 500, "snapshot", err.Error())
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", paf.MediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, meta.Name+".paf"))
	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

// requireSnapshotAccess lets the human through, and an agent only with
// agent.bundle.export on the snapshotted conversation.
func requireSnapshotAccess(w http.ResponseWriter, r *http.Request, meta snapshotlib.Meta, action string) bool {
	_, human, ok := authedCaller(w, r)
	if !ok {
		return false
	}
	if human {
		return true
	}
	if meta.ConvID == "" {
		writeError(w, http.StatusForbidden, "auth", "only the human operator may "+action+" whose agent is unknown")
		return false
	}
	_, ok = requireCrossAgentPermission(w, r, PermAgentBundleExport, meta.ConvID)
	return ok
}

// handleSnapshotRemove deletes a snapshot. Human-only.
//
//	DELETE /v1/snapshots/{name}
func handleSnapshotRemove(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "remove a snapshot") {
		return
	}
	name := r.PathValue("name")
	if !snapshotlib.ValidName(name) {
		writeError(w, 400, "name", fmt.Sprintf("invalid snapshot name %q", name))
		return
	}
	err := snapshotStore().Remove(name)
	switch {
	case errors.Is(err, snapshotlib.ErrNotFound):
		writeError(w, 404, "not_found", fmt.Sprintf("no snapshot %q", name))
	case err != nil:
		writeError(w, 500, "snapshot", err.Error())
	default:
		writeJSON(w, 200, map[string]any{"removed": name})
	}
}

// handleSnapshotPrune removes expired snapshots. Human-only.
//
//	POST /v1/snapshots/prune
func handleSnapshotPrune(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "prune snapshots") {
		return
	}
	removed, err := snapshotStore().Prune(time.Now())
	if err != nil {
		writeError(w, 500, "snapshot", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"removed": removed})
}
