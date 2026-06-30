package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the three paginated list endpoints the retired /
// conversations / replaced lists moved onto when they came off the 2s
// /api/snapshot poll: GET /api/retired, /api/conversations, /api/replaced.
// Each shares the offset/limit/q contract and the {rows,offset,limit,total,
// total_unfiltered} envelope; all windowing + filtering is server-side (SQL).

// dashListPage mirrors the shared paginated envelope. Generic over the row
// type so each endpoint reuses it.
type dashListPage[T any] struct {
	Rows            []T `json:"rows"`
	Offset          int `json:"offset"`
	Limit           int `json:"limit"`
	Total           int `json:"total"`
	TotalUnfiltered int `json:"total_unfiltered"`
}

// fetchListPage GETs a list endpoint and decodes the envelope.
func fetchListPage[T any](t *testing.T, mux http.Handler, path string) dashListPage[T] {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, rec.Code, "%s body=%s", path, rec.Body.String())
	var env dashListPage[T]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "decode %s", path)
	return env
}

// fetchListRows is fetchListPage keeping only the rows — used by
// fetchDashSnapshot to re-assemble the lists that left the snapshot.
func fetchListRows[T any](t *testing.T, mux http.Handler, path string) []T {
	t.Helper()
	return fetchListPage[T](t, mux, path).Rows
}

// convIDsOf projects a row slice onto its conv-ids via the accessor.
func convIDsOf[T any](rows []T, id func(T) string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, id(r))
	}
	return out
}

func retiredConvID(r dashRetired) string   { return r.ConvID }
func convConvID(c dashConversation) string { return c.ConvID }
func replacedConvID(r dashReplaced) string { return r.ConvID }

// setRetiredAt forces a retired actor's retired_at to a fixed RFC3339 stamp so
// the newest-first ordering is deterministic (HaveRetiredAgent stamps "now",
// which collides at second precision for fast back-to-back retires).
func setRetiredAt(t *testing.T, conv, ts string) {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE agents SET retired_at = ? WHERE current_conv_id = ?`, ts, conv)
	require.NoError(t, err)
}

// Requirement 1: /api/retired windows correctly — offset/limit slice the
// newest-first set, total/total_unfiltered are right, q narrows, and limit=0
// returns the FULL set (the modal "show all" path).
func TestDashboardRetired_PaginatesAndFilters(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const (
		r1 = "ret1-1111-2222-3333-444444444444"
		r2 = "ret2-1111-2222-3333-444444444444"
		r3 = "ret3-1111-2222-3333-444444444444"
	)
	f.HaveConvWithTitle(r1, "retired-one")
	f.HaveConvWithTitle(r2, "retired-two")
	f.HaveConvWithTitle(r3, "retired-three")
	f.HaveRetiredAgent(r1)
	f.HaveRetiredAgent(r2)
	f.HaveRetiredAgent(r3)
	// Pin retirement times so the newest-first order is r3, r2, r1.
	setRetiredAt(t, r1, "2024-01-01T00:00:01Z")
	setRetiredAt(t, r2, "2024-01-01T00:00:02Z")
	setRetiredAt(t, r3, "2024-01-01T00:00:03Z")

	// limit=0 → the WHOLE set (no LIMIT), served offset 0.
	full := fetchListPage[dashRetired](t, mux, "/api/retired?limit=0")
	assert.Equal(t, []string{r3, r2, r1}, convIDsOf(full.Rows, retiredConvID),
		"limit=0 returns the full newest-first set")
	assert.Equal(t, 3, full.Total, "total")
	assert.Equal(t, 3, full.TotalUnfiltered, "total_unfiltered")
	assert.Equal(t, 0, full.Offset, "served offset for unbounded is 0")
	assert.Equal(t, 0, full.Limit, "served limit echoes 0 (unbounded)")

	// First page of 2.
	p0 := fetchListPage[dashRetired](t, mux, "/api/retired?limit=2&offset=0")
	assert.Equal(t, []string{r3, r2}, convIDsOf(p0.Rows, retiredConvID), "page 0 is the 2 newest")
	assert.Equal(t, 3, p0.Total)
	assert.Equal(t, 2, p0.Limit)
	assert.Equal(t, 0, p0.Offset)

	// Second page of 2 — one row.
	p1 := fetchListPage[dashRetired](t, mux, "/api/retired?limit=2&offset=2")
	assert.Equal(t, []string{r1}, convIDsOf(p1.Rows, retiredConvID), "page 1 is the oldest")
	assert.Equal(t, 2, p1.Offset)

	// Offset past the end pulls back to the last full page (offset 2), never an
	// empty page.
	past := fetchListPage[dashRetired](t, mux, "/api/retired?limit=2&offset=10")
	assert.Equal(t, []string{r1}, convIDsOf(past.Rows, retiredConvID),
		"a stale offset past the end falls back to the last full page")
	assert.Equal(t, 2, past.Offset, "served offset is the clamped last-page offset")

	// q filters server-side (title here) — total reflects the match, while
	// total_unfiltered stays the whole set.
	q := fetchListPage[dashRetired](t, mux, "/api/retired?limit=0&q=two")
	assert.Equal(t, []string{r2}, convIDsOf(q.Rows, retiredConvID), "q=two matches only retired-two")
	assert.Equal(t, 1, q.Total, "total counts the q match")
	assert.Equal(t, 3, q.TotalUnfiltered, "total_unfiltered ignores q")
}

// Requirement 2: /api/conversations lists only NON-agent conversations
// (promotion candidates) and paginates them; an enrolled agent is excluded.
func TestDashboardConversations_ExcludesAgentsAndPaginates(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const (
		p1    = "pcv1-1111-2222-3333-444444444444"
		p2    = "pcv2-1111-2222-3333-444444444444"
		p3    = "pcv3-1111-2222-3333-444444444444"
		agent = "agnt-1111-2222-3333-444444444444"
	)
	// Plain convs with controlled file_mtime so newest-modified-first is
	// deterministic: p3, p2, p1.
	haveConv := func(conv, title string, mtime int64) {
		require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
			ConvID: conv, CustomTitle: title, FileMtime: mtime, IndexedAt: time.Now(),
		}))
	}
	haveConv(p1, "promote-one", 100)
	haveConv(p2, "promote-two", 200)
	haveConv(p3, "promote-three", 300)
	// An enrolled agent must NEVER appear as a promotion candidate.
	f.HaveConvWithTitle(agent, "real-agent")
	f.HaveEnrolledAgent(agent)

	full := fetchListPage[dashConversation](t, mux, "/api/conversations?limit=0")
	got := convIDsOf(full.Rows, convConvID)
	assert.Equal(t, []string{p3, p2, p1}, got, "only the 3 plain convs, newest-modified first")
	assert.NotContains(t, got, agent, "an enrolled agent is excluded from conversations")
	assert.Equal(t, 3, full.Total)
	assert.Equal(t, 3, full.TotalUnfiltered)

	// Paginate.
	p0 := fetchListPage[dashConversation](t, mux, "/api/conversations?limit=2&offset=0")
	assert.Equal(t, []string{p3, p2}, convIDsOf(p0.Rows, convConvID))
	assert.Equal(t, 3, p0.Total)
	last := fetchListPage[dashConversation](t, mux, "/api/conversations?limit=2&offset=2")
	assert.Equal(t, []string{p1}, convIDsOf(last.Rows, convConvID))

	// q narrows.
	q := fetchListPage[dashConversation](t, mux, "/api/conversations?limit=0&q=promote-two")
	assert.Equal(t, []string{p2}, convIDsOf(q.Rows, convConvID))
	assert.Equal(t, 1, q.Total)
	assert.Equal(t, 3, q.TotalUnfiltered)
}

// Requirement 3: /api/replaced returns superseded predecessors after a
// reincarnate, EXCLUDES the live head, annotates each with its actor, and a q
// filter narrows the set.
func TestDashboardReplaced_PredecessorsExcludeHeadAndFilter(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const (
		convX0 = "aaaa1111-2222-3333-4444-555555555555"
		labelX = "spwn-rl-x01"
		tmuxX  = "tclaude-spwn-rl-x01"
		cwdX   = "/tmp/rlx"
		convY0 = "bbbb2222-3333-4444-5555-666666666666"
		labelY = "spwn-rl-y01"
		tmuxY  = "tclaude-spwn-rl-y01"
		cwdY   = "/tmp/rly"
	)

	// Actor X: convX0 → reincarnate → convX1 (convX0 becomes a replaced gen).
	f.HaveConvWithTitle(convX0, "worker-x")
	f.HaveAliveSession(convX0, labelX, tmuxX, cwdX)
	f.HaveEnrolledAgent(convX0)
	rx := f.Reincarnate(convX0, "carry on")
	convX1 := rx.NewConv
	require.NotEmpty(t, convX1)
	actorX, err := db.AgentIDForConv(convX1)
	require.NoError(t, err)
	require.NotEmpty(t, actorX)

	// Actor Y: convY0 → reincarnate → convY1.
	f.HaveConvWithTitle(convY0, "worker-y")
	f.HaveAliveSession(convY0, labelY, tmuxY, cwdY)
	f.HaveEnrolledAgent(convY0)
	ry := f.Reincarnate(convY0, "carry on")
	convY1 := ry.NewConv
	require.NotEmpty(t, convY1)

	mux := agentd.BuildDashboardHandlerForTest()

	full := fetchListPage[dashReplaced](t, mux, "/api/replaced?limit=0")
	ids := convIDsOf(full.Rows, replacedConvID)
	assert.ElementsMatch(t, []string{convX0, convY0}, ids,
		"both predecessors surface; got %+v", full.Rows)
	assert.NotContains(t, ids, convX1, "the live head must NOT appear under replaced")
	assert.NotContains(t, ids, convY1, "the live head must NOT appear under replaced")
	assert.Equal(t, 2, full.Total)
	assert.Equal(t, 2, full.TotalUnfiltered)

	// The X predecessor row points back at its live actor + records the rotation.
	var rowX *dashReplaced
	for i := range full.Rows {
		if full.Rows[i].ConvID == convX0 {
			rowX = &full.Rows[i]
		}
	}
	require.NotNil(t, rowX, "convX0 row present")
	assert.Equal(t, convX1, rowX.ActorConvID, "predecessor points at the live head as its actor")
	assert.Equal(t, "reincarnate", rowX.Reason, "the rotation that superseded it")
	assert.False(t, rowX.ActorRetired, "the owning actor is active")

	// q narrows by predecessor conv-id (robust against the reincarnate display
	// rename of the predecessor's title).
	q := fetchListPage[dashReplaced](t, mux, "/api/replaced?limit=0&q=aaaa1111")
	assert.Equal(t, []string{convX0}, convIDsOf(q.Rows, replacedConvID), "q matches only the X predecessor")
	assert.Equal(t, 1, q.Total, "q match count")
	assert.Equal(t, 2, q.TotalUnfiltered, "total_unfiltered ignores q")
}

// Requirement 4: the snapshot no longer carries the retired / replaced /
// conversations JSON keys, yet the roster still EXCLUDES retired actors and
// superseded predecessors from agents[]/ungrouped[].
func TestDashboardSnapshot_DropsMovedListsButStillGuardsRoster(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	// A retired agent.
	const retired = "rtrd-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(retired, "retired-worker")
	f.HaveRetiredAgent(retired)

	// A superseded predecessor (reincarnate leaves convP behind under head convH).
	const (
		convP  = "cccc1111-2222-3333-4444-555555555555"
		labelP = "spwn-rl-p01"
		tmuxP  = "tclaude-spwn-rl-p01"
		cwdP   = "/tmp/rlp"
	)
	f.HaveConvWithTitle(convP, "live-worker")
	f.HaveAliveSession(convP, labelP, tmuxP, cwdP)
	f.HaveEnrolledAgent(convP)
	rp := f.Reincarnate(convP, "carry on")
	convH := rp.NewConv
	require.NotEmpty(t, convH)

	// The raw /api/snapshot body must not carry the three moved keys.
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code, "snapshot body=%s", rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), "decode raw snapshot")
	for _, gone := range []string{"retired", "replaced", "conversations"} {
		_, present := raw[gone]
		assert.False(t, present, "snapshot must no longer carry the %q key", gone)
	}

	// Roster still guards: neither the retired actor nor the superseded
	// predecessor reaches agents[]/ungrouped[]; the live head does.
	snap := fetchDashSnapshot(t, mux)
	assert.False(t, agentInSnap(snap.Agents, retired), "retired actor must not be on the roster")
	assert.False(t, agentInSnap(snap.Agents, convP), "superseded predecessor must not be on the roster")
	assert.False(t, ungroupedHas(snap, convP), "superseded predecessor must not be ungrouped")
	assert.True(t, agentInSnap(snap.Agents, convH), "the live head IS the agent on the roster")
}
