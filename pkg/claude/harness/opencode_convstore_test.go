package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func openCodeTestSessions() []openCodeSession {
	return []openCodeSession{
		{
			ID:        "ses_alpha111",
			Title:     "Alpha native title",
			Created:   1784808076886,
			Updated:   1784808077386,
			ProjectID: "project-a",
			Directory: "/work/a",
		},
		{
			ID:        "ses_alpha222",
			Title:     "Second alpha",
			Created:   1784808077886,
			Updated:   1784808078386,
			ProjectID: "project-b",
			Directory: "/work/b",
		},
	}
}

func openCodeTestStore(sessions []openCodeSession) openCodeConvStore {
	return openCodeConvStore{listSessions: func(string) ([]openCodeSession, error) {
		return sessions, nil
	}}
}

func TestOpenCodeConvStore_ListConvsMapsDirectoryAndCaches(t *testing.T) {
	withTestDB(t)
	store := openCodeTestStore(openCodeTestSessions())

	all, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "ses_alpha111", all[0].SessionID)
	assert.Equal(t, "/work/a", all[0].ProjectPath)
	assert.Equal(t, "Alpha native title", all[0].Summary)
	assert.Equal(t, OpenCodeName, all[0].Harness)
	assert.Equal(t, "2026-07-23T12:01:16Z", all[0].Created)
	assert.Equal(t, time.UnixMilli(1784808077386).UTC(), all[0].FileMtime)

	local, err := store.ListConvs("/work/a")
	require.NoError(t, err)
	require.Len(t, local, 1)
	assert.Equal(t, "ses_alpha111", local[0].SessionID)

	row, err := db.GetConvIndex("ses_alpha111")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, OpenCodeName, row.Harness)
	assert.Equal(t, "/work/a", row.ProjectPath)
	assert.Equal(t, "Alpha native title", row.Summary)
}

func TestOpenCodeConvStore_ListConvsPreservesProjectedBranch(t *testing.T) {
	withTestDB(t)
	const convID = "ses_alpha111"
	require.NoError(t, db.UpsertConvIndexBranchSnapshot(&db.ConvIndexRow{
		ConvID: convID, ProjectPath: "/work/a", Harness: OpenCodeName,
		GitBranch: "feature-opencode", GitBranchStartup: "main",
	}))

	_, err := openCodeTestStore(openCodeTestSessions()).ListConvs("")
	require.NoError(t, err)
	row, err := db.GetConvIndex(convID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "feature-opencode", row.GitBranch,
		"the branch projected from the runtime cwd survives a cold-store refresh")
	assert.Equal(t, "main", row.GitBranchStartup)
}

func TestOpenCodeFileMtime_AboveUnixNanoRangeSkipsCacheLoudlyWithoutHidingSession(t *testing.T) {
	withTestDB(t)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	const maxUnixNanoMillis = int64(^uint64(0)>>1) / int64(time.Millisecond)
	session := openCodeSession{ID: "ses_out_of_range", Directory: "/work", Updated: maxUnixNanoMillis + 1}
	entry := openCodeSessionEntry(session)
	require.False(t, entry.FileMtime.IsZero(), "an invalid present instant must not become the absence sentinel")

	entries, err := openCodeTestStore([]openCodeSession{session}).ListConvs("")
	require.NoError(t, err, "one bad third-party timestamp must not blank the source listing")
	require.Len(t, entries, 1)
	assert.Equal(t, session.ID, entries[0].SessionID)

	row, err := db.GetConvIndex(session.ID)
	require.NoError(t, err)
	assert.Nil(t, row, "an unrepresentable timestamp must skip its cache row")
	assert.Contains(t, logs.String(), "level=ERROR")
	assert.Contains(t, logs.String(), session.ID)
	assert.Contains(t, logs.String(), strconv.FormatInt(session.Updated, 10),
		"the loud failure identifies the offending third-party value")
	assert.Contains(t, logs.String(), "outside the Unix-nanosecond range")
}

func TestOpenCodeConvStore_GenericCacheFailureDoesNotHideCLISessions(t *testing.T) {
	withTestDB(t)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TRIGGER reject_opencode_cache_fixture
		BEFORE INSERT ON conv_index
		WHEN NEW.conv_id = 'ses_cache_failure'
		BEGIN
			SELECT RAISE(ABORT, 'generic cache failure fixture');
		END`)
	require.NoError(t, err)

	session := openCodeSession{
		ID:        "ses_cache_failure",
		Title:     "Still visible from the CLI",
		Created:   1784808076886,
		Updated:   1784808077386,
		Directory: "/work/cache-failure",
	}
	entries, err := openCodeTestStore([]openCodeSession{session}).ListConvs("")
	require.NoError(t, err, "a generic cache failure must not suppress source CLI sessions")
	require.Len(t, entries, 1)
	assert.Equal(t, session.ID, entries[0].SessionID)
	assert.Equal(t, session.Title, entries[0].Summary)

	row, err := db.GetConvIndex(session.ID)
	require.NoError(t, err)
	assert.Nil(t, row, "the listing came from the CLI even though its cache write failed")

	// Pin the split direction, not just the degrade. Skipping the cache row and
	// still listing the session holds on the representation-error arm too, so
	// without a level assertion a change that misclassified ordinary transient
	// cache failures as representation failures would log every one of them at
	// ERROR on a deliberately quiet path, and no test would fail.
	// Matched against the full record rather than a bare "level=WARN", so
	// unrelated migration logging in the same buffer can neither satisfy nor
	// break the assertion.
	assert.Contains(t, logs.String(),
		`level=WARN msg="opencode convstore: conv_index upsert failed; continuing from CLI"`)
	assert.NotContains(t, logs.String(), `level=ERROR msg="opencode convstore:`,
		"an ordinary cache failure is a quiet degrade, not a loud representation failure")
	assert.Contains(t, logs.String(), session.ID)
	assert.Contains(t, logs.String(), "generic cache failure fixture")
}

func TestOpenCodeConvStore_ResolveLocalGlobalAndAmbiguous(t *testing.T) {
	withTestDB(t)
	store := openCodeTestStore(openCodeTestSessions())

	ref, err := store.Resolve("ses_alpha111", "/work/a", false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, "/work/a", ref.ProjectPath)
	assert.Equal(t, OpenCodeName, ref.Harness)

	ref, err = store.Resolve("ses_alpha2", "/work/a", false)
	require.NoError(t, err)
	assert.Nil(t, ref, "local resolve must not reach another cwd")

	ref, err = store.Resolve("ses_alpha2", "/work/a", true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, "ses_alpha222", ref.ConvID)

	ref, err = store.Resolve("ses_alpha", "/work/a", true)
	require.Error(t, err)
	assert.Nil(t, ref)
	assert.Contains(t, err.Error(), "ambiguous")
}

func TestOpenCodeConvStore_TitleExistsAndColdRenameRoundTrip(t *testing.T) {
	withTestDB(t)
	store := openCodeTestStore(openCodeTestSessions())

	title, err := store.Title("ses_alpha111")
	require.NoError(t, err)
	assert.Equal(t, "Alpha native title", title)

	ok, err := store.Exists("ses_alpha111", "/work/a")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = store.Exists("ses_alpha111", "/work/b")
	require.NoError(t, err)
	assert.True(t, ok, "globally indexed OpenCode ids must ignore caller cwd")

	require.NoError(t, store.SetTitle("ses_alpha111", "Cold local rename"))
	title, err = store.Title("ses_alpha111")
	require.NoError(t, err)
	assert.Equal(t, "Cold local rename", title)

	row, err := db.GetConvIndex("ses_alpha111")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "Cold local rename", row.CustomTitle)

	err = store.SetTitle("ses_missing", "No phantom")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestOpenCodeConvStore_LiveRenamePrefersAPI(t *testing.T) {
	withTestDB(t)
	sessions := openCodeTestSessions()
	require.NoError(t, db.SetConvIndexCustomTitle(
		"ses_alpha111", "Stale cold rename", OpenCodeName))
	called := false
	store := openCodeConvStore{
		listSessions: func(string) ([]openCodeSession, error) { return sessions, nil },
		writeTitle: func(runtime db.OpenCodeRuntime, convID, title string) error {
			called = true
			assert.Equal(t, "ses_alpha111", runtime.ConvID)
			assert.Equal(t, "ses_alpha111", convID)
			assert.Equal(t, "Live rename", title)
			sessions[0].Title = title
			return nil
		},
	}
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "session-a",
		ConvID:    "ses_alpha111",
		ServerURL: "http://127.0.0.1:43210",
		Password:  "secret",
		PID:       1234,
		Cwd:       "/work/a",
		CreatedAt: time.Now(),
	}))

	require.NoError(t, store.SetTitle("ses_alpha111", "Live rename"))
	assert.True(t, called)
	row, err := db.GetConvIndex("ses_alpha111")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "Live rename", row.CustomTitle,
		"live success replaces any stale cold-title override")

	title, err := store.Title("ses_alpha111")
	require.NoError(t, err)
	assert.Equal(t, "Live rename", title)
	row, err = db.GetConvIndex("ses_alpha111")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Empty(t, row.CustomTitle,
		"native title convergence clears the temporary cache override")
}

func TestWriteOpenCodeTitleUsesAuthenticatedManagedAPI(t *testing.T) {
	const password = "private-password"
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, "opencode", user)
		assert.Equal(t, password, pass)
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/session/ses_alpha111", r.URL.Path)
		assert.Equal(t, "/work/a", r.URL.Query().Get("directory"))
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "API rename", body["title"])
		_, _ = w.Write([]byte(`{"id":"ses_alpha111","title":"API rename"}`))
	}))
	defer server.Close()

	err := writeOpenCodeTitle(db.OpenCodeRuntime{
		ConvID:    "ses_alpha111",
		ServerURL: server.URL,
		Password:  password,
		PID:       os.Getpid(),
		Cwd:       "/work/a",
	}, "ses_alpha111", "API rename")
	require.NoError(t, err)
	assert.True(t, called)
}

func TestOpenCodeConvStore_LiveRenameFallsBackToCache(t *testing.T) {
	withTestDB(t)
	store := openCodeConvStore{
		listSessions: func(string) ([]openCodeSession, error) {
			return nil, errors.New("CLI should not be needed for known runtime")
		},
		writeTitle: func(db.OpenCodeRuntime, string, string) error {
			return errors.New("server stopped")
		},
	}
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "session-a", ConvID: "ses_alpha111",
		ServerURL: "http://127.0.0.1:43210", Password: "secret",
		PID: 1234, Cwd: "/work/a",
	}))

	require.NoError(t, store.SetTitle("ses_alpha111", "Cached after stop"))
	row, err := db.GetConvIndex("ses_alpha111")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "Cached after stop", row.CustomTitle)
}

func TestOpenCodeConvStoreRejectsMalformedCLIShape(t *testing.T) {
	_, err := parseOpenCodeSessions([]byte(`[{"id":"ses_ok"}]`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lacks id or directory")

	empty, err := parseOpenCodeSessions(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestOpenCodeConvStoreResolveSkipsImpossibleIDWithoutCLI(t *testing.T) {
	store := openCodeConvStore{listSessions: func(string) ([]openCodeSession, error) {
		return nil, errors.New("must not run for a non-OpenCode id")
	}}
	ref, err := store.Resolve("019ec004", "/work/a", true)
	require.NoError(t, err)
	assert.Nil(t, ref)
}

// `opencode session list` shows only one project, so a snapshot evicts what is
// gone from the directories it covers and leaves other projects' rows alone.
func TestOpenCodeSyncKeepsRowsOutsideTheSnapshot(t *testing.T) {
	withTestDB(t)
	for id, dir := range map[string]string{"ses_gone": "/repo/a", "ses_other": "/repo/b"} {
		require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: id, ProjectDir: dir, ProjectPath: dir, Harness: OpenCodeName}))
	}
	_, err := syncOpenCodeConvIndex([]openCodeSession{{ID: "ses_here", Directory: "/repo/a"}}, time.Now().Add(time.Hour))
	require.NoError(t, err)

	gone, err := db.GetConvIndex("ses_gone")
	require.NoError(t, err)
	assert.Nil(t, gone)
	other, err := db.GetConvIndex("ses_other")
	require.NoError(t, err)
	assert.NotNil(t, other)
}

// `opencode session list` shows one project, so a member landed in a repository
// the daemon does not run from is only found by listing from there.
func TestOpenCodeConvStoreFindsConversationInAnotherProject(t *testing.T) {
	withTestDB(t)
	repo := testutil.CanonicalTempDir(t)
	const conv = "ses_landed1"
	require.NoError(t, db.SetConversationResumeProfile(conv, db.ConversationResumeProfile{
		Version: db.RelaunchProfileVersion, Harness: OpenCodeName, Cwd: repo,
	}))
	var (
		mu    sync.Mutex
		asked []string
	)
	store := openCodeConvStore{listSessions: func(dir string) ([]openCodeSession, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, dir)
		if dir == repo {
			return []openCodeSession{{ID: conv, Title: "Landed", Directory: repo}}, nil
		}
		return nil, nil
	}}

	exists, err := store.Exists(conv, "")
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, []string{repo}, asked, "a known conversation takes one listing")

	ref, err := store.Resolve(conv, "", true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, repo, ref.ProjectPath)
	assert.Equal(t, []string{repo, repo}, asked, "nor does resolving its full id")

	ref, err = store.Resolve("ses_land", "", true)
	require.NoError(t, err)
	require.NotNil(t, ref, "a prefix lists every project")

	require.NoError(t, store.SetTitle(conv, "Renamed before resume"))
	title, err := store.Title(conv)
	require.NoError(t, err)
	assert.Equal(t, "Renamed before resume", title)
}

// A landed conversation in private state is in no listing. It can still be
// renamed, and a listing of its directory does not read it as deleted.
func TestOpenCodeConvStoreKeepsPrivateStateConversation(t *testing.T) {
	withTestDB(t)
	repo := testutil.CanonicalTempDir(t)
	const conv = "ses_private1"
	require.NoError(t, db.SetConversationResumeProfile(conv, db.ConversationResumeProfile{
		Version: db.RelaunchProfileVersion, Harness: OpenCodeName, Cwd: repo,
	}))
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	_, err = db.InsertOpenCodeAgentStateAllocation(db.OpenCodeAgentStateAllocation{
		AgentID: agentID, Mode: db.OpenCodeStatePrivate, StateRoot: t.TempDir(),
	})
	require.NoError(t, err)
	store := openCodeConvStore{listSessions: func(string) ([]openCodeSession, error) {
		return []openCodeSession{{ID: "ses_ambient", Directory: repo}}, nil
	}}

	require.NoError(t, store.SetTitle(conv, "Private name"))
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: conv, ProjectDir: repo, ProjectPath: repo, CustomTitle: "Private name", Harness: OpenCodeName}))
	_, err = store.ListConvs(repo)
	require.NoError(t, err)

	title, err := store.Title(conv)
	require.NoError(t, err)
	assert.Equal(t, "Private name", title)
}

// One project's listing failing does not hide the others, and a project that
// is gone is not asked at all.
func TestOpenCodeConvStoreStoreWideListingSurvivesOneProject(t *testing.T) {
	withTestDB(t)
	good, bad := testutil.CanonicalTempDir(t), testutil.CanonicalTempDir(t)
	for id, dir := range map[string]string{"ses_good": good, "ses_bad": bad, "ses_vanished": filepath.Join(bad, "gone")} {
		require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: id, ProjectDir: dir, ProjectPath: dir, Harness: OpenCodeName}))
	}
	store := openCodeConvStore{listSessions: func(dir string) ([]openCodeSession, error) {
		switch dir {
		case good:
			return []openCodeSession{{ID: "ses_good", Directory: good}}, nil
		case bad:
			return nil, errors.New("cannot list")
		case filepath.Join(bad, "gone"):
			t.Errorf("listed from %q", dir)
		}
		return nil, nil
	}}

	entries, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "ses_good", entries[0].SessionID)
	row, err := db.GetConvIndex("ses_bad")
	require.NoError(t, err)
	assert.NotNil(t, row, "unseen is not gone")
}

// One answer covers its project, so its other directories are not asked.
func TestOpenCodeConvStoreListsAProjectOnce(t *testing.T) {
	withTestDB(t)
	root := testutil.CanonicalTempDir(t)
	var dirs []string
	var answer []openCodeSession
	for i := range 9 {
		dir := filepath.Join(root, strconv.Itoa(i))
		require.NoError(t, os.Mkdir(dir, 0o755))
		id := "ses_" + strconv.Itoa(i)
		require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: id, ProjectDir: dir, ProjectPath: dir, Harness: OpenCodeName}))
		dirs = append(dirs, dir)
		answer = append(answer, openCodeSession{ID: id, Directory: dir})
	}
	var calls atomic.Int32
	store := openCodeConvStore{listSessions: func(dir string) ([]openCodeSession, error) {
		calls.Add(1)
		if slices.Contains(dirs, dir) {
			return answer, nil
		}
		return nil, nil
	}}

	entries, err := store.ListConvs("")
	require.NoError(t, err)
	assert.Len(t, entries, 9)
	assert.LessOrEqual(t, int(calls.Load()), openCodeListConcurrency+1)
}

// A second identical listing writes nothing, so a watcher of indexed_at rests.
func TestOpenCodeSyncDoesNotRewriteAnUnchangedRow(t *testing.T) {
	withTestDB(t)
	store := openCodeTestStore(openCodeTestSessions())
	_, err := store.ListConvs("")
	require.NoError(t, err)
	before, err := db.GetConvIndex("ses_alpha111")
	require.NoError(t, err)
	require.NotNil(t, before)

	_, err = store.ListConvs("")
	require.NoError(t, err)
	after, err := db.GetConvIndex("ses_alpha111")
	require.NoError(t, err)
	assert.Equal(t, before.IndexedAt, after.IndexedAt)
}

// A row written while a listing ran was too new for the snapshot to show.
func TestOpenCodeSyncKeepsRowWrittenDuringTheListing(t *testing.T) {
	withTestDB(t)
	listedAt := time.Now().Add(-time.Minute)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: "ses_new", ProjectDir: "/repo/a", ProjectPath: "/repo/a", Harness: OpenCodeName}))
	_, err := syncOpenCodeConvIndex([]openCodeSession{{ID: "ses_here", Directory: "/repo/a"}}, listedAt)
	require.NoError(t, err)
	row, err := db.GetConvIndex("ses_new")
	require.NoError(t, err)
	assert.NotNil(t, row)
}

func TestListOpenCodeSessionsRunsFromTheProject(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	executable := filepath.Join(testutil.CanonicalTempDir(t), "opencode")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nprintf '[{\"id\":\"ses_here\",\"directory\":\"%s\"}]' \"$PWD\"\n"), 0o755))

	sessions, err := listOpenCodeSessions(executable, dir)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, dir, sessions[0].Directory)
}
