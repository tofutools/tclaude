package agentd_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests exercise the live conv_index monitor (fsnotify.go)
// against a real fsnotify watcher over a tmpdir HOME's
// ~/.claude/projects tree. The monitor is a background goroutine with
// no HTTP surface — its input is the filesystem and its output is the
// conv_index SQLite cache — so these are subsystem integration tests,
// not daemon-mux flow tests: input is real .jsonl writes via a CCSim,
// the assertion is the production read surface db.GetConvIndex, and no
// explicit `conv ls` ever runs. Live events are async, so their assertions
// poll via require.Eventually; startup assertions use the scan-complete
// barrier exposed by the monitor.

// requireMonitor starts the monitor against the test HOME and skips
// (rather than fails) when fsnotify is unavailable — startConvMonitor
// degrades to nil there, and a platform without inotify/kqueue is an
// environment limitation, not a regression.
func requireMonitor(t *testing.T, debounce time.Duration) {
	t.Helper()
	m := agentd.StartConvMonitorForTest(t, debounce)
	if m == nil {
		t.Skip("fsnotify watcher unavailable in this environment")
	}
	agentd.WaitForConvMonitorStartupForTest(t, m)
}

// Scenario: a conversation that already existed before the monitor
// started — fsnotify only delivers events for future changes, so the
// monitor's startup scan is what indexes it. Covers a stale / empty
// conv_index after a daemon restart.
func TestConvMonitor_StartupScanIndexesExistingConvs(t *testing.T) {
	w := testharness.New(t)

	const convID = "fsst0001-2222-3333-4444-555555555555"
	cwd := filepath.Join(w.HomeDir, "git", "preexisting")
	cc := testharness.NewCCSimWithID(t, w.HomeDir, convID, cwd)
	require.NoError(t, cc.Start())
	require.NoError(t, cc.WriteUserTurn("a prompt written before the daemon started"))

	// Nothing has scanned this .jsonl yet — conv_index has no row.
	if row, _ := db.GetConvIndex(convID); row != nil {
		t.Fatalf("precondition: conv_index already has a row for %s", convID)
	}

	// Monitor start runs a one-time scan of every existing .jsonl.
	requireMonitor(t, 15*time.Millisecond)

	require.Eventually(t, func() bool {
		row, _ := db.GetConvIndex(convID)
		return row != nil && row.FirstPrompt != ""
	}, 10*time.Second, 10*time.Millisecond,
		"the startup scan must index a conv that existed before the monitor started")
}

// Scenario: tclaude was offline while a conv's .jsonl kept being
// appended to (e.g. the human kept using Claude Code with the daemon
// down). The cached conv_index row is stale — its FileMtime/FileSize
// no longer match what's on disk. The startup scan's freshness guard
// must spot the mismatch and reparse, so the row catches up to the
// real file content. Repair-on-restart behaviour.
func TestConvMonitor_StartupScanRefreshesChangedConv(t *testing.T) {
	w := testharness.New(t)

	const convID = "fsst0002-2222-3333-4444-555555555555"
	cwd := filepath.Join(w.HomeDir, "git", "changed-while-down")
	cc := testharness.NewCCSimWithID(t, w.HomeDir, convID, cwd)
	require.NoError(t, cc.Start())
	require.NoError(t, cc.WriteUserTurn("first prompt added while tclaude was down"))

	// Plant a stale conv_index row — as if a prior tclaude indexed this
	// conv at one point and recorded an older mtime/size, then the
	// daemon went down while CC kept appending. Both fields disagree
	// with the current on-disk file.
	info, err := os.Stat(cc.JsonlPath)
	require.NoError(t, err)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  filepath.Dir(cc.JsonlPath),
		FullPath:    cc.JsonlPath,
		FileMtime:   info.ModTime().Unix() - 3600, // 1h behind
		FileSize:    0,                            // and size mismatched
		FirstPrompt: "STALE — should be replaced on startup",
		// Created must be non-empty or isStubRow() treats this row as a
		// stub and the freshness guard re-scans unconditionally — which
		// would still let the test pass, but for the wrong reason. Set
		// it so the only thing driving the reparse is mtime/size.
		Created:   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		IndexedAt: time.Now(),
	}))

	requireMonitor(t, 15*time.Millisecond)

	require.Eventually(t, func() bool {
		row, _ := db.GetConvIndex(convID)
		return row != nil && row.FirstPrompt == "first prompt added while tclaude was down"
	}, 10*time.Second, 10*time.Millisecond,
		"startup scan must reparse a .jsonl whose on-disk mtime/size moved past the cached row")
}

// Scenario: the opposite of the previous test — the cached row's
// FileMtime/FileSize match the on-disk .jsonl exactly (nothing changed
// while tclaude was down). The startup scan's freshness guard must
// SKIP the reparse: this is the "don't burn CPU reparsing hundreds of
// inert convs on every daemon boot" property. Verified by planting a
// sentinel Summary that the real file's content would never produce,
// and proving the sentinel survives the startup scan.
func TestConvMonitor_StartupScanSkipsUnchangedConv(t *testing.T) {
	w := testharness.New(t)

	const convID = "fsfr0001-2222-3333-4444-555555555555"
	cwd := filepath.Join(w.HomeDir, "git", "unchanged")
	cc := testharness.NewCCSimWithID(t, w.HomeDir, convID, cwd)
	require.NoError(t, cc.Start())

	info, err := os.Stat(cc.JsonlPath)
	require.NoError(t, err)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:     convID,
		ProjectDir: filepath.Dir(cc.JsonlPath),
		FullPath:   cc.JsonlPath,
		FileMtime:  info.ModTime().Unix(),
		FileSize:   info.Size(),
		Summary:    "freshness-guard-sentinel",
		// Non-empty Created so isStubRow() returns false — otherwise
		// RefreshConvIndexEntry treats this as a stub and re-scans
		// regardless of mtime, defeating the test.
		Created:   time.Now().UTC().Format(time.RFC3339),
		IndexedAt: time.Now(),
	}))

	requireMonitor(t, 15*time.Millisecond)

	// requireMonitor returned only after the single-threaded startup scan
	// completed, so the sentinel can now be checked directly: a failed guard
	// would already have replaced it with CCSim's actual summary.
	row, err := db.GetConvIndex(convID)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, "freshness-guard-sentinel", row.Summary,
		"startup scan must NOT reparse a .jsonl whose mtime+size match the cached row")
}

// Scenario: a write to a live conversation's .jsonl — the kind a
// /rename produces — must land in conv_index on its own. The monitor
// sees the Write, debounces it, and ScanAndUpsertFile's the fresh row.
func TestConvMonitor_WriteRefreshesConvIndex(t *testing.T) {
	w := testharness.New(t)

	const convID = "fsmw0001-2222-3333-4444-555555555555"
	cwd := filepath.Join(w.HomeDir, "git", "demo")
	cc := testharness.NewCCSimWithID(t, w.HomeDir, convID, cwd)
	require.NoError(t, cc.Start()) // materialises the project dir + .jsonl

	requireMonitor(t, 15*time.Millisecond)

	// The startup scan indexes the pre-existing conv first — it has no
	// custom title yet.
	require.Eventually(t, func() bool {
		row, _ := db.GetConvIndex(convID)
		return row != nil
	}, 10*time.Second, 10*time.Millisecond, "startup scan must index the existing conv")

	// A /rename-style write to the live .jsonl — the title transition
	// from "" proves the live fsnotify Write path, not the startup scan.
	require.NoError(t, cc.WriteCustomTitle("monitored-title"))

	require.Eventually(t, func() bool {
		row, _ := db.GetConvIndex(convID)
		return row != nil && row.CustomTitle == "monitored-title"
	}, 10*time.Second, 10*time.Millisecond,
		"conv_index must reflect the live .jsonl write without any explicit conv ls")
}

// Scenario: a brand-new conversation — a project dir + .jsonl that did
// not exist when the monitor started — must still be indexed. The root
// watch catches the new project dir; the monitor Add()s it and scans
// what it contains.
func TestConvMonitor_NewConvIsIndexed(t *testing.T) {
	w := testharness.New(t)

	// Monitor starts first, on an effectively empty projects tree —
	// startConvMonitor creates ~/.claude/projects and watches the root.
	requireMonitor(t, 15*time.Millisecond)

	// A brand-new conversation appears under a not-yet-watched project
	// dir: CCSim.Start() creates the subdir, WriteUserTurn fills in a
	// first prompt.
	const convID = "fsnc0001-2222-3333-4444-555555555555"
	cwd := filepath.Join(w.HomeDir, "git", "fresh")
	cc := testharness.NewCCSimWithID(t, w.HomeDir, convID, cwd)
	require.NoError(t, cc.Start())
	require.NoError(t, cc.WriteUserTurn("first prompt for the fresh conv"))

	require.Eventually(t, func() bool {
		row, _ := db.GetConvIndex(convID)
		return row != nil && row.FirstPrompt != ""
	}, 10*time.Second, 10*time.Millisecond,
		"a brand-new conversation .jsonl must be picked up by the monitor")
}

// Scenario: deleting a conversation's .jsonl must drop its conv_index
// row. ScanAndUpsertFile is self-cleaning — an os.Stat miss deletes the
// row — so the monitor's Remove path needs no special-casing; this pins
// that it actually reaches it.
func TestConvMonitor_RemoveDeletesConvIndexRow(t *testing.T) {
	w := testharness.New(t)

	const convID = "fsrm0001-2222-3333-4444-555555555555"
	cwd := filepath.Join(w.HomeDir, "git", "doomed")
	cc := testharness.NewCCSimWithID(t, w.HomeDir, convID, cwd)
	require.NoError(t, cc.Start())
	require.NoError(t, cc.WriteUserTurn("doomed conv"))

	requireMonitor(t, 15*time.Millisecond)

	// The startup scan indexes it.
	require.Eventually(t, func() bool {
		row, _ := db.GetConvIndex(convID)
		return row != nil
	}, 10*time.Second, 10*time.Millisecond, "conv must be indexed first")

	// Delete the .jsonl — the monitor must drop the conv_index row.
	require.NoError(t, os.Remove(cc.JsonlPath))
	require.Eventually(t, func() bool {
		row, _ := db.GetConvIndex(convID)
		return row == nil
	}, 10*time.Second, 10*time.Millisecond,
		"conv_index row must be deleted when the .jsonl is removed")
}
