package harness_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The Codex ConvStore is exercised here through its REAL surface — the
// ConvStore interface registered on the codex Harness — rather than the
// unexported helpers, so this is an external (harness_test) package. That
// also breaks the import cycle the internal test package would hit
// (testharness → pkg/claude/session → harness).

const codexName = "codex"

// codexConvs returns the registered Codex ConvStore.
func codexConvs(t *testing.T) harness.ConvStore {
	t.Helper()
	h, ok := harness.Get(codexName)
	require.True(t, ok, "codex harness must be registered")
	require.NotNil(t, h.Convs)
	return h.Convs
}

// codexTestHome makes a throwaway HOME the Codex read path (os.UserHomeDir)
// and CodexSim both resolve to. Both HOME and USERPROFILE are set for
// cross-platform parity.
func codexTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// codexThreadSeed is a test-side view of the `threads` columns the read
// path consumes. Mirrors the real Codex state-DB row a renamed/aged
// session would carry.
type codexThreadSeed struct {
	ID               string
	Cwd              string
	Title            string
	GitBranch        string
	Model            string
	FirstUserMessage string
	Preview          string
	CreatedAt        int64
	UpdatedAt        int64
	Archived         bool
	ArchivedAt       sql.NullInt64
}

// writeCodexThread seeds a `threads` row in ~/.codex/state_5.sqlite.
// CodexSim deliberately does NOT write the state DB (it only owns the
// rollout .jsonl), so the enrichment path is tested by laying a row down
// next to the sim's rollout. The schema is the verified-real column subset
// the read path SELECTs.
func writeCodexThread(t *testing.T, home string, tr codexThreadSeed) {
	t.Helper()
	path := filepath.Join(home, ".codex", "state_5.sqlite")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	d, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`CREATE TABLE IF NOT EXISTS threads (
		id TEXT PRIMARY KEY,
		rollout_path TEXT NOT NULL DEFAULT '',
		cwd TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		git_branch TEXT,
		model TEXT,
		first_user_message TEXT,
		preview TEXT,
		tokens_used INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0,
		archived INTEGER NOT NULL DEFAULT 0,
		archived_at INTEGER
	)`)
	require.NoError(t, err)

	archived := 0
	if tr.Archived {
		archived = 1
	}
	var archivedAt any
	if tr.ArchivedAt.Valid {
		archivedAt = tr.ArchivedAt.Int64
	}
	_, err = d.Exec(`INSERT INTO threads
		(id, rollout_path, cwd, title, git_branch, model, first_user_message,
		 preview, tokens_used, created_at, updated_at, archived, archived_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		tr.ID, "", tr.Cwd, tr.Title, tr.GitBranch, tr.Model,
		tr.FirstUserMessage, tr.Preview, 0, tr.CreatedAt, tr.UpdatedAt,
		archived, archivedAt)
	require.NoError(t, err)
}

// startCodexSim creates, starts, and writes one full exchange into a Codex
// rollout under home/cwd, returning the sim (its ConvID is the rollout id).
func startCodexSim(t *testing.T, home, convID, cwd, prompt, reply string) *testharness.CodexSim {
	t.Helper()
	cx := testharness.NewCodexSimWithID(t, home, convID, cwd)
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteExchange(prompt, reply))
	return cx
}

func findEntry(entries []convops.SessionEntry, id string) (convops.SessionEntry, bool) {
	for _, e := range entries {
		if e.SessionID == id {
			return e, true
		}
	}
	return convops.SessionEntry{}, false
}

// --- rollout-only path (no threads DB) -------------------------------------

func TestCodexConvStore_ListConvs_RolloutOnly(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const id = "019ec004-4250-79b1-9ade-ebaea4135453"
	cx := startCodexSim(t, home, id, cwd, "hello world", "hi there")

	entries, err := codexConvs(t).ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)

	e := entries[0]
	assert.Equal(t, id, e.SessionID)
	assert.Equal(t, "hello world", e.FirstPrompt)
	assert.Equal(t, cwd, e.ProjectPath, "cwd comes from session_meta")
	assert.Equal(t, "gpt-5.5", e.Model, "model comes from turn_context")
	assert.Equal(t, codexName, e.Harness)
	assert.Empty(t, e.CustomTitle, "no threads row ⇒ no rename signal ⇒ derived title only")
	assert.NotEmpty(t, e.Created, "Created derived from session_meta timestamp")
	_, perr := time.Parse(time.RFC3339, e.Created)
	assert.NoError(t, perr, "Created is RFC3339")
	assert.NotEmpty(t, e.Modified)
	assert.Equal(t, cx.RolloutPath, e.FullPath, "FullPath is the rollout path")
}

func TestCodexConvStore_ListConvs_CwdFilter(t *testing.T) {
	home := codexTestHome(t)
	cwdA, cwdB := "/home/u/a", "/home/u/b"
	idA := "aaaa1111-1111-1111-1111-111111111111"
	idB := "bbbb2222-2222-2222-2222-222222222222"
	startCodexSim(t, home, idA, cwdA, "in a", "ok")
	startCodexSim(t, home, idB, cwdB, "in b", "ok")
	cs := codexConvs(t)

	all, err := cs.ListConvs("")
	require.NoError(t, err)
	assert.Len(t, all, 2, "empty cwd lists every conv")

	onlyA, err := cs.ListConvs(cwdA)
	require.NoError(t, err)
	require.Len(t, onlyA, 1)
	assert.Equal(t, idA, onlyA[0].SessionID)
	assert.Equal(t, cwdA, onlyA[0].ProjectPath)
}

func TestCodexConvStore_ListConvs_NoSessionsDir(t *testing.T) {
	codexTestHome(t) // ~/.codex/sessions never created
	entries, err := codexConvs(t).ListConvs("")
	require.NoError(t, err, "absent rollout tree is not an error")
	assert.Empty(t, entries)
}

// --- threads enrichment path -----------------------------------------------

func TestCodexConvStore_ThreadsEnrichment_Rename(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const id = "cccc3333-3333-3333-3333-333333333333"
	startCodexSim(t, home, id, cwd, "hello", "hi")

	writeCodexThread(t, home, codexThreadSeed{
		ID:               id,
		Cwd:              cwd,
		Title:            "Renamed Title", // != first_user_message ⇒ rename
		GitBranch:        "feature/x",
		Model:            "gpt-5.5",
		FirstUserMessage: "hello",
		Preview:          "hello",
		CreatedAt:        1781337965,
		UpdatedAt:        1781337973,
	})

	entries, err := codexConvs(t).ListConvs("")
	require.NoError(t, err)
	e, ok := findEntry(entries, id)
	require.True(t, ok)
	assert.Equal(t, "Renamed Title", e.CustomTitle, "native rename ⇒ CustomTitle")
	assert.Equal(t, "hello", e.FirstPrompt)
	assert.Equal(t, "feature/x", e.GitBranch, "branch from threads.git_branch")
	assert.Equal(t, cwd, e.ProjectPath)
	assert.Equal(t, "gpt-5.5", e.Model)
	assert.Equal(t, "2026-06-13T08:06:05Z", e.Created, "Created from threads.created_at (UTC RFC3339)")
	assert.False(t, e.IsArchived())
}

func TestCodexConvStore_ThreadsEnrichment_Derived(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const id = "dddd4444-4444-4444-4444-444444444444"
	startCodexSim(t, home, id, cwd, "hello", "hi")

	// title == first_user_message: Codex's auto-derived title, NOT a rename.
	writeCodexThread(t, home, codexThreadSeed{
		ID:               id,
		Cwd:              cwd,
		Title:            "hello",
		FirstUserMessage: "hello",
		Preview:          "hello",
		CreatedAt:        1781337965,
		UpdatedAt:        1781337973,
	})

	entries, err := codexConvs(t).ListConvs("")
	require.NoError(t, err)
	e, ok := findEntry(entries, id)
	require.True(t, ok)
	assert.Empty(t, e.CustomTitle, "derived title ⇒ no CustomTitle")
	assert.Equal(t, "hello", e.FirstPrompt)
}

// A long/multi-line first message whose threads.title is just the trimmed
// first line is Codex's auto-title, not a rename — the heuristic must not
// flag it (the false-positive window codexIsRename narrows).
func TestCodexConvStore_ThreadsEnrichment_DerivedLongMessage(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const id = "eeee5555-5555-5555-5555-555555555555"
	fullMsg := "fix the parser bug\nwith more detail on the second line"
	startCodexSim(t, home, id, cwd, fullMsg, "ok")

	writeCodexThread(t, home, codexThreadSeed{
		ID:               id,
		Cwd:              cwd,
		Title:            "fix the parser bug", // == first line of fullMsg
		FirstUserMessage: fullMsg,
		Preview:          "fix the parser bug",
		CreatedAt:        1781337965,
		UpdatedAt:        1781337973,
	})

	entries, err := codexConvs(t).ListConvs("")
	require.NoError(t, err)
	e, ok := findEntry(entries, id)
	require.True(t, ok)
	assert.Empty(t, e.CustomTitle, "auto-title from a long message must not read as a rename")
	assert.Equal(t, fullMsg, e.FirstPrompt, "FirstPrompt keeps the full message")
}

func TestCodexConvStore_ThreadsEnrichment_Archived(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const id = "ffff6666-6666-6666-6666-666666666666"
	startCodexSim(t, home, id, cwd, "hello", "hi")

	const archivedAtSec = int64(1781340000)
	writeCodexThread(t, home, codexThreadSeed{
		ID:               id,
		Cwd:              cwd,
		Title:            "hello",
		FirstUserMessage: "hello",
		CreatedAt:        1781337965,
		UpdatedAt:        1781337973,
		Archived:         true,
		ArchivedAt:       sql.NullInt64{Int64: archivedAtSec, Valid: true},
	})

	entries, err := codexConvs(t).ListConvs("")
	require.NoError(t, err)
	e, ok := findEntry(entries, id)
	require.True(t, ok)
	assert.True(t, e.IsArchived())
	assert.Equal(t, time.Unix(archivedAtSec, 0).UTC().Format(time.RFC3339), e.ArchivedAt)
}

// On a drifted/sparse schema where a row leaves columns NULL, a single
// malformed row must NOT abort the whole load and silently degrade every
// conversation to rollout-only — the bad row degrades alone, others keep
// their enrichment. (Builds its own permissive table since the real schema
// marks these columns NOT NULL, so the failure mode only appears on drift.)
func TestCodexConvStore_ThreadsEnrichment_NullRowTolerated(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const goodID = "10101010-1010-1010-1010-101010101010"
	const badID = "20202020-2020-2020-2020-202020202020"
	startCodexSim(t, home, goodID, cwd, "hello good", "hi")
	startCodexSim(t, home, badID, cwd, "hello bad", "hi")

	path := filepath.Join(home, ".codex", "state_5.sqlite")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	d, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY, rollout_path TEXT, cwd TEXT, title TEXT,
		git_branch TEXT, model TEXT, first_user_message TEXT, preview TEXT,
		tokens_used INTEGER, created_at INTEGER, updated_at INTEGER,
		archived INTEGER, archived_at INTEGER)`)
	require.NoError(t, err)
	// A fully-populated row (rename + branch) — its enrichment must survive.
	_, err = d.Exec(`INSERT INTO threads VALUES
		(?, '', ?, 'Renamed Good', 'br-good', 'gpt-5.5', 'hello good', 'hello good',
		 0, 1781337965, 1781337973, 0, NULL)`, goodID, cwd)
	require.NoError(t, err)
	// A degenerate row with NULLs in columns the pre-fix scan read into
	// plain string/int64 — that scan would error and abort loadCodexThreads.
	_, err = d.Exec(`INSERT INTO threads
		(id, rollout_path, cwd, title, git_branch, model, first_user_message,
		 preview, tokens_used, created_at, updated_at, archived, archived_at)
		VALUES (?, NULL, ?, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)`,
		badID, cwd)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := codexConvs(t).ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 2, "both convs still listed despite the malformed row")

	good, ok := findEntry(entries, goodID)
	require.True(t, ok)
	assert.Equal(t, "Renamed Good", good.CustomTitle, "the good row's enrichment survived the bad row")
	assert.Equal(t, "br-good", good.GitBranch)

	_, ok = findEntry(entries, badID)
	assert.True(t, ok, "the malformed-row conv is still listed (degraded, not dropped)")
}

// --- Resolve ---------------------------------------------------------------

func TestCodexConvStore_Resolve(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	startCodexSim(t, home, "abc11111-1111-1111-1111-111111111111", cwd, "one", "ok")
	startCodexSim(t, home, "abc22222-2222-2222-2222-222222222222", cwd, "two", "ok")
	startCodexSim(t, home, "def33333-3333-3333-3333-333333333333", cwd, "three", "ok")
	cs := codexConvs(t)

	// Exact id wins over the shared "abc" prefix.
	ref, err := cs.Resolve("abc11111-1111-1111-1111-111111111111", cwd, false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, cwd, ref.ProjectPath)
	assert.Equal(t, codexName, ref.Harness)

	// Unique prefix → resolves.
	ref, err = cs.Resolve("def", cwd, false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, "def33333-3333-3333-3333-333333333333", ref.ConvID)

	// No match → (nil, nil).
	ref, err = cs.Resolve("zzz", cwd, false)
	require.NoError(t, err)
	assert.Nil(t, ref)

	// Ambiguous prefix → error, not collapsed into not-found.
	ref, err = cs.Resolve("abc", cwd, false)
	require.Error(t, err)
	assert.Nil(t, ref)
	assert.Contains(t, err.Error(), "ambiguous")

	// Empty prefix → (nil, nil).
	ref, err = cs.Resolve("", cwd, false)
	require.NoError(t, err)
	assert.Nil(t, ref)
}

func TestCodexConvStore_Resolve_LocalVsGlobal(t *testing.T) {
	home := codexTestHome(t)
	cwdA, cwdB := "/home/u/a", "/home/u/b"
	startCodexSim(t, home, "aaaa1111-1111-1111-1111-111111111111", cwdA, "in a", "ok")
	startCodexSim(t, home, "bbbb2222-2222-2222-2222-222222222222", cwdB, "in b", "ok")
	cs := codexConvs(t)

	// Local resolve in cwdA cannot see cwdB's conv.
	ref, err := cs.Resolve("bbbb", cwdA, false)
	require.NoError(t, err)
	assert.Nil(t, ref, "local resolve must not reach another project")

	// Global resolve finds it.
	ref, err = cs.Resolve("bbbb", cwdA, true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, cwdB, ref.ProjectPath)
}

// --- Title -----------------------------------------------------------------

func TestCodexConvStore_Title(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const renamedID = "11110000-0000-0000-0000-000000000000"
	const derivedID = "22220000-0000-0000-0000-000000000000"
	const rolloutOnlyID = "33330000-0000-0000-0000-000000000000"
	startCodexSim(t, home, renamedID, cwd, "hello", "hi")
	startCodexSim(t, home, derivedID, cwd, "what is up", "hi")
	startCodexSim(t, home, rolloutOnlyID, cwd, "rollout only title", "hi")

	writeCodexThread(t, home, codexThreadSeed{
		ID: renamedID, Cwd: cwd, Title: "A Renamed Convo",
		FirstUserMessage: "hello", CreatedAt: 1781337965, UpdatedAt: 1781337973,
	})
	writeCodexThread(t, home, codexThreadSeed{
		ID: derivedID, Cwd: cwd, Title: "what is up",
		FirstUserMessage: "what is up", CreatedAt: 1781337965, UpdatedAt: 1781337973,
	})

	cs := codexConvs(t)

	// Rename → threads.title.
	got, err := cs.Title(renamedID)
	require.NoError(t, err)
	assert.Equal(t, "A Renamed Convo", got)

	// Derived (title == first_user_message) → the first user message.
	got, err = cs.Title(derivedID)
	require.NoError(t, err)
	assert.Equal(t, "what is up", got)

	// No threads row → derived from the rollout's first user message.
	got, err = cs.Title(rolloutOnlyID)
	require.NoError(t, err)
	assert.Equal(t, "rollout only title", got)

	// Unknown conv → ("", nil).
	got, err = cs.Title("99990000-0000-0000-0000-000000000000")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// --- cold .zst rollout (no threads row) ------------------------------------

// A threads-less session that has aged to `.jsonl.zst` must still assemble
// by streaming the decompressed rollout — the only path that actually
// decompresses (an enriched .zst is served from the threads row instead).
func TestCodexConvStore_ColdZstRollout(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const id = "77778888-8888-8888-8888-888888888888"
	cx := startCodexSim(t, home, id, cwd, "compressed hello", "hi")

	// Compress the rollout the sim wrote to <path>.zst and drop the plain
	// .jsonl, leaving only the cold form on disk.
	plain := cx.RolloutPath
	raw, err := os.ReadFile(plain)
	require.NoError(t, err)
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	compressed := enc.EncodeAll(raw, nil)
	require.NoError(t, enc.Close())
	require.NoError(t, os.WriteFile(plain+".zst", compressed, 0o644))
	require.NoError(t, os.Remove(plain))

	entries, err := codexConvs(t).ListConvs("")
	require.NoError(t, err)
	e, ok := findEntry(entries, id)
	require.True(t, ok, "cold .zst rollout must be listed")
	assert.Equal(t, "compressed hello", e.FirstPrompt, "decompressed and parsed")
	assert.Equal(t, cwd, e.ProjectPath)
}

// During Codex's hot→cold compression window a session's .jsonl and
// .jsonl.zst coexist under the same uuid. The store must list the conv ONCE
// (preferring the uncompressed file) and a unique-uuid prefix must NOT come
// back as a spurious "ambiguous" Resolve.
func TestCodexConvStore_DedupHotColdWindow(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const id = "abcdef01-2345-6789-abcd-ef0123456789"
	cx := startCodexSim(t, home, id, cwd, "mid compression", "hi")

	// Write <path>.zst NEXT TO the live .jsonl (Codex compresses first,
	// deletes the .jsonl only afterwards).
	plain := cx.RolloutPath
	raw, err := os.ReadFile(plain)
	require.NoError(t, err)
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(plain+".zst", enc.EncodeAll(raw, nil), 0o644))
	require.NoError(t, enc.Close())

	cs := codexConvs(t)
	entries, err := cs.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1, "the conv is listed exactly once, not once per file")
	assert.Equal(t, plain, entries[0].FullPath, "the uncompressed .jsonl is preferred over the .zst")

	// Exact id resolves.
	ref, err := cs.Resolve(id, cwd, false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, id, ref.ConvID)

	// A prefix that uniquely names the uuid must NOT be a spurious ambiguous.
	ref, err = cs.Resolve("abcdef01", cwd, false)
	require.NoError(t, err, "unique uuid prefix must not be a spurious ambiguous error")
	require.NotNil(t, ref)
	assert.Equal(t, id, ref.ConvID)
}

// A corrupt cold rollout (garbage bytes, no .jsonl twin, no threads row)
// must be skipped with a warning — never crash the listing or fail the
// whole scan; healthy convs alongside it still list.
func TestCodexConvStore_CorruptZstSkipped(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const goodID = "00aa00aa-00aa-00aa-00aa-00aa00aa00aa"
	startCodexSim(t, home, goodID, cwd, "i am fine", "hi")

	const badID = "00bb00bb-00bb-00bb-00bb-00bb00bb00bb"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "06", "13")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	bad := filepath.Join(dir, "rollout-2026-06-13T10-00-00-"+badID+".jsonl.zst")
	require.NoError(t, os.WriteFile(bad, []byte("this is not valid zstd data at all"), 0o644))

	entries, err := codexConvs(t).ListConvs("")
	require.NoError(t, err)
	_, ok := findEntry(entries, goodID)
	assert.True(t, ok, "the healthy conv is listed despite a corrupt sibling")
	_, ok = findEntry(entries, badID)
	assert.False(t, ok, "the corrupt rollout is skipped, not surfaced")
}

// --- descriptor wiring -----------------------------------------------------

func TestCodexHarness_Registered(t *testing.T) {
	h, ok := harness.Get(codexName)
	require.True(t, ok, "codex harness must be registered")
	require.NotNil(t, h.Convs, "codex harness must expose a ConvStore")
	assert.Equal(t, "Codex CLI", h.DisplayName)
	// Rename/compact stay unsupported (Codex has no in-pane rename — titles
	// live in its threads state DB, reached via ConvStore/JOH-161 — and
	// compact is unwired), but soft-exit is supported: JOH-160 added a
	// `/quit` Lifecycle command so the daemon can stop a Codex agent
	// gracefully.
	assert.False(t, h.SupportsRename())
	assert.False(t, h.SupportsCompact())
	assert.True(t, h.SupportsSoftExit())
}
