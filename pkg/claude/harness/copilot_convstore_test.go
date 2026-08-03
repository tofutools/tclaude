package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The synthetic trees below are shaped from the REAL 1.0.77 files recorded by
// pkg/claude/harness/copilotfixture (see the ConvStore smoke test there, which
// drives this same production code against the actual binary's output). These
// tests exist for the cases a live CLI cannot be asked to produce on demand:
// a corrupt workspace file, a truncated event log, an ambiguous id prefix.

// copilotSession writes one session-state directory.
func copilotSession(t *testing.T, home, id, workspace string, eventLines ...string) {
	t.Helper()
	dir := filepath.Join(home, copilotSessionStateDirName, id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if workspace != "" {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, copilotWorkspaceFileName), []byte(workspace), 0o644))
	}
	if len(eventLines) > 0 {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, copilotEventsFileName),
			[]byte(strings.Join(eventLines, "\n")+"\n"), 0o644))
	}
}

// workspaceYAML renders the exact key set 1.0.77 writes.
func workspaceYAML(id, cwd, name string, userNamed bool, created, updated string) string {
	named := "false"
	if userNamed {
		named = "true"
	}
	return "id: " + id + "\n" +
		"cwd: " + cwd + "\n" +
		"git_root: " + cwd + "\n" +
		"repository: octo/example\n" +
		"host_type: github\n" +
		"branch: probe-branch\n" +
		"client_name: github/cli\n" +
		"name: " + name + "\n" +
		"user_named: " + named + "\n" +
		"summary_count: 0\n" +
		"created_at: " + created + "\n" +
		"updated_at: " + updated + "\n"
}

const (
	copilotTestID  = "11111111-2222-4333-8444-555555555555"
	copilotTestID2 = "11111111-2222-4333-8444-666666666666"
	copilotOtherID = "99999999-2222-4333-8444-555555555555"
)

func copilotStartEvent(id, cwd, model string) string {
	return `{"type":"session.start","id":"e1","parentId":null,` +
		`"timestamp":"2026-08-03T19:08:12.435Z","data":{"sessionId":"` + id +
		`","selectedModel":"` + model + `","context":{"cwd":"` + cwd + `"}}}`
}

func copilotUserEvent(content string) string {
	return `{"type":"user.message","id":"e2","parentId":"e1",` +
		`"timestamp":"2026-08-03T19:08:12.579Z","data":{"content":"` + content +
		`","delivery":"idle"}}`
}

// copilotSystemEvent stands in for the ~26 kB system prompt line the CLI
// writes once per turn — the line the scan's prefilter must skip.
func copilotSystemEvent() string {
	return `{"type":"system.message","id":"e0","parentId":null,` +
		`"timestamp":"2026-08-03T19:08:12.500Z","data":{"content":"` +
		strings.Repeat("You are the GitHub Copilot CLI. ", 200) + `"}}`
}

func TestCopilotConvStoreListsSessions(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "first prompt about widgets", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotTestID, cwd, "gpt-5.1-codex"),
		copilotSystemEvent(),
		copilotUserEvent("first prompt about widgets"),
		copilotUserEvent("second prompt about gadgets"),
	)
	store := copilotConvStore{home: home}

	entries, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	entry := entries[0]

	assert.Equal(t, copilotTestID, entry.SessionID)
	assert.Equal(t, cwd, entry.ProjectPath)
	assert.Equal(t, CopilotName, entry.Harness)
	assert.Equal(t, "first prompt about widgets", entry.FirstPrompt)
	assert.Equal(t, 2, entry.MessageCount,
		"every user.message is a turn, including the ones a resume appended")
	assert.Equal(t, "gpt-5.1-codex", entry.Model)
	assert.Equal(t, "probe-branch", entry.GitBranch)
	assert.Equal(t, "2026-08-03T19:08:12Z", entry.Created)
	assert.Equal(t, "2026-08-03T19:08:13Z", entry.Modified)
	assert.Equal(t, filepath.Join(home, copilotSessionStateDirName, copilotTestID,
		copilotEventsFileName), entry.FullPath)
	assert.False(t, entry.FileMtime.IsZero())
	assert.Positive(t, entry.FileSize)
}

// A Copilot-generated name is a summary; only `user_named: true` is an
// operator's own title. DisplayTitle's existing precedence then needs no
// Copilot-specific rule.
func TestCopilotConvStoreTitleSplitsGeneratedFromUserNamed(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "generated summary", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotUserEvent("the raw prompt"))
	copilotSession(t, home, copilotTestID2,
		workspaceYAML(copilotTestID2, cwd, "My Named Session", true,
			"2026-08-03T19:07:34.750Z", "2026-08-03T19:07:34.808Z"),
		copilotUserEvent("hello there"))
	store := copilotConvStore{home: home}

	entries, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	byID := map[string]int{entries[0].SessionID: 0, entries[1].SessionID: 1}

	generated := entries[byID[copilotTestID]]
	assert.Equal(t, "generated summary", generated.Summary)
	assert.Empty(t, generated.CustomTitle)

	named := entries[byID[copilotTestID2]]
	assert.Equal(t, "My Named Session", named.CustomTitle)
	assert.Empty(t, named.Summary)

	title, err := store.Title(copilotTestID2)
	require.NoError(t, err)
	assert.Equal(t, "My Named Session", title)

	title, err = store.Title(copilotTestID)
	require.NoError(t, err)
	assert.Equal(t, "generated summary", title)

	title, err = store.Title("no-such-conversation")
	require.NoError(t, err)
	assert.Empty(t, title, "an unknown conv is an empty title, not an error")
}

// A session with no name yet falls back to its first prompt.
func TestCopilotConvStoreTitleFallsBackToFirstPrompt(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotUserEvent("what do you make of this"))

	title, err := copilotConvStore{home: home}.Title(copilotTestID)
	require.NoError(t, err)
	assert.Equal(t, "what do you make of this", title)
}

func TestCopilotConvStoreFiltersByCwd(t *testing.T) {
	home := t.TempDir()
	mine, other := t.TempDir(), t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, mine, "mine", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	copilotSession(t, home, copilotOtherID,
		workspaceYAML(copilotOtherID, other, "theirs", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	store := copilotConvStore{home: home}

	entries, err := store.ListConvs(mine)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, copilotTestID, entries[0].SessionID)

	// A trailing separator is the same directory.
	entries, err = store.ListConvs(mine + string(filepath.Separator))
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	entries, err = store.ListConvs("")
	require.NoError(t, err)
	assert.Len(t, entries, 2, "the empty cwd sentinel means every working directory")
}

func TestCopilotConvStoreListsNewestFirst(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "older", false,
			"2026-08-03T19:00:00.000Z", "2026-08-03T19:00:01.000Z"))
	copilotSession(t, home, copilotOtherID,
		workspaceYAML(copilotOtherID, cwd, "newer", false,
			"2026-08-03T19:10:00.000Z", "2026-08-03T19:10:01.000Z"))

	entries, err := copilotConvStore{home: home}.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, copilotOtherID, entries[0].SessionID)
	assert.Equal(t, copilotTestID, entries[1].SessionID)
}

func TestCopilotConvStoreResolve(t *testing.T) {
	home := t.TempDir()
	cwd, other := t.TempDir(), t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "mine", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	copilotSession(t, home, copilotTestID2,
		workspaceYAML(copilotTestID2, other, "theirs", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	store := copilotConvStore{home: home}

	// Exact id, globally.
	ref, err := store.Resolve(copilotTestID, "", true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, copilotTestID, ref.ConvID)
	assert.Equal(t, cwd, ref.ProjectPath)
	assert.Equal(t, CopilotName, ref.Harness)

	// The shared prefix of two conversations is ambiguous, which is an ERROR
	// rather than "not found" — the caller must be told to be more specific.
	_, err = store.Resolve("11111111-2222", "", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")

	// The same prefix is unambiguous once cwd narrows the candidates.
	ref, err = store.Resolve("11111111-2222", cwd, false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, copilotTestID, ref.ConvID)

	// global widens past a cwd that does not contain the conversation.
	ref, err = store.Resolve(copilotTestID2, cwd, true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, copilotTestID2, ref.ConvID)

	// Not found is (nil, nil), distinguishable from both error cases.
	ref, err = store.Resolve("deadbeef", "", true)
	require.NoError(t, err)
	assert.Nil(t, ref)

	ref, err = store.Resolve("", "", true)
	require.NoError(t, err)
	assert.Nil(t, ref)
}

func TestCopilotConvStoreExists(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "mine", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	store := copilotConvStore{home: home}

	exists, err := store.Exists(copilotTestID, cwd)
	require.NoError(t, err)
	assert.True(t, exists)

	// cwd is ignored: session-state is indexed by id alone.
	exists, err = store.Exists(copilotTestID, t.TempDir())
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = store.Exists(copilotOtherID, cwd)
	require.NoError(t, err)
	assert.False(t, exists)

	exists, err = store.Exists("", cwd)
	require.NoError(t, err)
	assert.False(t, exists)

	// An id that is not a single path segment can never be a session id, and
	// must not be joined onto the session-state path.
	exists, err = store.Exists("../../etc", cwd)
	require.NoError(t, err)
	assert.False(t, exists)
}

// A session Copilot created but has not written workspace.yaml into yet is not
// a conversation. It must be skipped, not reported as a nameless one.
func TestCopilotConvStoreSkipsDirectoryWithoutWorkspace(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotOtherID, "")
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "real", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))

	store := copilotConvStore{home: home}
	entries, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, copilotTestID, entries[0].SessionID)

	exists, err := store.Exists(copilotOtherID, cwd)
	require.NoError(t, err)
	assert.False(t, exists, "Exists must agree with ListConvs about what is a conversation")
}

// One corrupt session must not hide the healthy ones.
func TestCopilotConvStoreSkipsCorruptSessionsWithoutFailingTheListing(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotOtherID, "id: [unterminated\n\tbad: :\n")
	copilotSession(t, home, copilotTestID2,
		"id: "+copilotTestID2+"\nname: no cwd here\nuser_named: false\n")
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "healthy", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))

	entries, err := copilotConvStore{home: home}.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, copilotTestID, entries[0].SessionID)
}

// A live session's last line is routinely half-written. The scan keeps the
// events it did read and the workspace metadata stays intact.
func TestCopilotConvStoreToleratesTruncatedEventLog(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "live session", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotTestID, cwd, "gpt-5.1-codex"),
		copilotUserEvent("complete prompt"),
		`{"type":"user.message","data":{"content":"half writt`,
	)

	entries, err := copilotConvStore{home: home}.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "complete prompt", entries[0].FirstPrompt)
	assert.Equal(t, 1, entries[0].MessageCount)
	assert.Equal(t, "gpt-5.1-codex", entries[0].Model)
	assert.Equal(t, "live session", entries[0].Summary)
}

// A session with no event log at all still lists from workspace.yaml alone.
func TestCopilotConvStoreListsWithoutAnEventLog(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "just created", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))

	entries, err := copilotConvStore{home: home}.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "just created", entries[0].Summary)
	assert.Empty(t, entries[0].FirstPrompt)
	assert.Zero(t, entries[0].MessageCount)
	assert.True(t, entries[0].FileMtime.IsZero())
}

// A resume restates the model in force; last one wins.
func TestCopilotConvStoreTakesModelFromTheLatestSessionEvent(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "resumed", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotTestID, cwd, "first-model"),
		copilotUserEvent("one"),
		`{"type":"session.resume","id":"e9","parentId":"e2",`+
			`"timestamp":"2026-08-03T19:08:13.220Z","data":{"selectedModel":"second-model",`+
			`"eventCount":8,"context":{"cwd":"`+cwd+`"}}}`,
		`{"type":"session.model_change","id":"e10","parentId":"e9",`+
			`"timestamp":"2026-08-03T19:08:13.221Z","data":{"newModel":"third-model",`+
			`"previousModel":"second-model"}}`,
		copilotUserEvent("two"),
	)

	entries, err := copilotConvStore{home: home}.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "third-model", entries[0].Model)
	assert.Equal(t, 2, entries[0].MessageCount)
}

// A COPILOT_HOME the CLI has never run under is an empty listing, not a
// failure — the same answer a fresh install gives.
func TestCopilotConvStoreEmptyOnFreshHome(t *testing.T) {
	store := copilotConvStore{home: filepath.Join(t.TempDir(), "never-used")}

	entries, err := store.ListConvs("")
	require.NoError(t, err)
	assert.Empty(t, entries)

	ref, err := store.Resolve(copilotTestID, "", true)
	require.NoError(t, err)
	assert.Nil(t, ref)

	exists, err := store.Exists(copilotTestID, "")
	require.NoError(t, err)
	assert.False(t, exists)
}

// SetTitle is refused: agentd renames Copilot through the `/rename` injection,
// and workspace.yaml is Copilot-managed state.
func TestCopilotConvStoreSetTitleIsRefused(t *testing.T) {
	err := copilotConvStore{home: t.TempDir()}.SetTitle(copilotTestID, "new title")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/rename")
}

// The registered descriptor must expose the store, and must still expose the
// in-pane rename that makes SetTitle unreachable through agentd's dispatch.
func TestCopilotDescriptorExposesConvStore(t *testing.T) {
	h, ok := Get(CopilotName)
	require.True(t, ok)
	require.True(t, h.SupportsConvs())
	assert.True(t, h.SupportsRename(),
		"a harness whose ConvStore refuses SetTitle must rename via injection")
}

// The event scan skips lines it cannot possibly care about, so a session's
// system prompts are never decoded.
func TestCopilotEventPrefilter(t *testing.T) {
	assert.False(t, copilotEventLineOfInterest([]byte(copilotSystemEvent())))
	assert.True(t, copilotEventLineOfInterest([]byte(copilotUserEvent("hi"))))
	assert.True(t, copilotEventLineOfInterest(
		[]byte(copilotStartEvent(copilotTestID, "/tmp", "m"))))
	// A false positive is harmless: the decode below it rejects the line.
	assert.True(t, copilotEventLineOfInterest(
		[]byte(`{"type":"assistant.message","data":{"content":"what is user.message?"}}`)))
}

func TestCopilotTimestampNormalization(t *testing.T) {
	assert.Equal(t, "2026-08-03T19:08:12Z", copilotTimestamp("2026-08-03T19:08:12.442Z"))
	assert.Equal(t, "2026-08-03T17:08:12Z", copilotTimestamp("2026-08-03T19:08:12.442+02:00"))
	assert.Empty(t, copilotTimestamp(""))
	assert.Empty(t, copilotTimestamp("   "))
	// Unparsable values pass through rather than vanishing.
	assert.Equal(t, "not a timestamp", copilotTimestamp("not a timestamp"))
}

// A tool result larger than the line limit must cost ONE line, not the tail of
// the log. events.jsonl is append-only, so a scan that gave up on the oversized
// line would freeze this conversation's turn count and model permanently.
func TestCopilotConvStoreRecoversFromAnOversizedEventLine(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	oversized := `{"type":"tool.execution_complete","data":{"content":"` +
		strings.Repeat("x", copilotEventLineLimit+1024) + `"}}`
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "big session", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotTestID, cwd, "first-model"),
		copilotUserEvent("before the big line"),
		oversized,
		copilotUserEvent("after the big line"),
		`{"type":"session.model_change","id":"eX","parentId":"e2",`+
			`"timestamp":"2026-08-03T19:08:13.221Z","data":{"newModel":"later-model"}}`,
	)

	entries, err := copilotConvStore{home: home}.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, 2, entries[0].MessageCount,
		"the turn after an oversized line must still be counted")
	assert.Equal(t, "later-model", entries[0].Model,
		"a model change after an oversized line must still be seen")
	assert.Equal(t, "before the big line", entries[0].FirstPrompt)
}

// An oversized line that also happens to contain a prefilter needle must not
// be decoded or counted — it is dropped whole.
func TestCopilotConvStoreDropsAnOversizedLineWithoutCountingIt(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "big session", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		`{"type":"user.message","data":{"content":"`+
			strings.Repeat("y", copilotEventLineLimit+1024)+`"}}`,
		copilotUserEvent("the only countable turn"),
	)

	entries, err := copilotConvStore{home: home}.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, 1, entries[0].MessageCount)
	assert.Equal(t, "the only countable turn", entries[0].FirstPrompt)
}

// Exists and ListConvs must share one definition of "is a conversation".
// A workspace.yaml that parses but carries no cwd is not one.
func TestCopilotConvStoreExistsAgreesWithListOnUnusableWorkspace(t *testing.T) {
	home := t.TempDir()
	store := copilotConvStore{home: home}

	copilotSession(t, home, copilotOtherID, "id: [unterminated\n\tbad: :\n")
	copilotSession(t, home, copilotTestID2,
		"id: "+copilotTestID2+"\nname: no cwd here\nuser_named: false\n")

	entries, err := store.ListConvs("")
	require.NoError(t, err)
	require.Empty(t, entries)

	for _, id := range []string{copilotOtherID, copilotTestID2} {
		exists, err := store.Exists(id, "")
		require.NoError(t, err)
		assert.False(t, exists,
			"a workspace ListConvs skips must not be reported as present: %s", id)
	}
}

// Resolve and Title must not pay for the event logs they do not read. The
// probe here is an events.jsonl that is a DIRECTORY: opening it fails, so a
// code path that scanned it would log and degrade, while one that never
// touches it is unaffected — and neither result changes what these two return.
func TestCopilotConvStoreResolveAndTitleSkipTheEventLog(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "named from workspace", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	require.NoError(t, os.Mkdir(filepath.Join(home, copilotSessionStateDirName,
		copilotTestID, copilotEventsFileName), 0o755))
	store := copilotConvStore{home: home}

	ref, err := store.Resolve(copilotTestID[:8], "", true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, copilotTestID, ref.ConvID)

	title, err := store.Title(copilotTestID)
	require.NoError(t, err)
	assert.Equal(t, "named from workspace", title)

	exists, err := store.Exists(copilotTestID, "")
	require.NoError(t, err)
	assert.True(t, exists)
}

// Title still falls back to the event log's first prompt when workspace.yaml
// has no name — the one case that genuinely needs the scan.
func TestCopilotConvStoreTitleReadsEventLogOnlyForTheFallback(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotUserEvent("the fallback prompt"))

	title, err := copilotConvStore{home: home}.Title(copilotTestID)
	require.NoError(t, err)
	assert.Equal(t, "the fallback prompt", title)
}

// Archiving is a tclaude concept Copilot has no equivalent for, so it lives in
// conv_index alone. Without the overlay `IsArchived()` is false for every
// Copilot conversation and `conv ls` would never actually hide one.
func TestCopilotConvStoreOverlaysArchivedState(t *testing.T) {
	withTestDB(t)
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "archived one", false,
			"2026-08-03T19:00:00.000Z", "2026-08-03T19:00:01.000Z"))
	copilotSession(t, home, copilotOtherID,
		workspaceYAML(copilotOtherID, cwd, "active one", false,
			"2026-08-03T19:10:00.000Z", "2026-08-03T19:10:01.000Z"))
	store := copilotConvStore{home: home}

	// Nothing archived yet.
	entries, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for i := range entries {
		assert.False(t, entries[i].IsArchived())
	}

	// A conv_index row is what `tclaude conv archive` stamps.
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:     copilotTestID,
		ProjectDir: cwd,
		Harness:    CopilotName,
		IndexedAt:  time.Now(),
	}))
	require.NoError(t, db.SetConvIndexArchived(copilotTestID, true))

	entries, err = store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 2, "an archived conv is still listed; callers filter it")
	byID := map[string]*convops.SessionEntry{}
	for i := range entries {
		byID[entries[i].SessionID] = &entries[i]
	}
	assert.True(t, byID[copilotTestID].IsArchived(),
		"conv ls hides a row via IsArchived(), which reads ArchivedAt")
	assert.NotEmpty(t, byID[copilotTestID].ArchivedAt)
	assert.False(t, byID[copilotOtherID].IsArchived())

	// Unarchiving clears it again.
	require.NoError(t, db.SetConvIndexArchived(copilotTestID, false))
	entries, err = store.ListConvs("")
	require.NoError(t, err)
	for i := range entries {
		assert.False(t, entries[i].IsArchived())
	}
}

// An archived row belonging to ANOTHER harness must never bleed onto a Copilot
// conversation that happens to share an id.
func TestCopilotConvStoreIgnoresArchivedRowsOfOtherHarnesses(t *testing.T) {
	withTestDB(t)
	home := t.TempDir()
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "mine", false,
			"2026-08-03T19:00:00.000Z", "2026-08-03T19:00:01.000Z"))

	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:     copilotTestID,
		ProjectDir: cwd,
		Harness:    CodexName,
		IndexedAt:  time.Now(),
	}))
	require.NoError(t, db.SetConvIndexArchived(copilotTestID, true))

	entries, err := copilotConvStore{home: home}.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.False(t, entries[0].IsArchived())
}
