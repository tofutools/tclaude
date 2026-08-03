package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// spawnFormOnWorktree opens the spawn form on an operator console with the
// worktree picker already asking for a new worktree — the state the operator
// reaches by cycling the picker once.
func spawnFormOnWorktree(t *testing.T, api tuiAPI) tuiModel {
	t.Helper()
	m := newTUIModel(api)
	m.operator = true
	m.width = 120
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m = m.openSpawnForm()
	m = m.focusSpawnField(tuiFieldWorktree).cycleChoice(1)
	require.True(t, m.creatingWorktree(), "the picker should be on %q", tuiWorktreeNew)
	return m
}

// typeInto sends one field's worth of keystrokes through the ordinary key
// path, so the tests exercise what the operator's fingers do rather than
// SetValue, which no keystroke goes through.
func typeInto(m tuiModel, field int, text string) tuiModel {
	m = m.focusSpawnField(field)
	for _, r := range text {
		updated, _ := m.handleSpawnKey(tuiKey(string(r)))
		m = updated.(tuiModel)
	}
	return m
}

// The branch a new worktree is cut on defaults to the agent's own name: an
// operator who names the agent has named its branch too.
func TestTUISpawnWorktreeBranchFollowsTheName(t *testing.T) {
	m := spawnFormOnWorktree(t, nil)
	m = typeInto(m, tuiFieldName, "reviewer")
	assert.Equal(t, "reviewer", m.form.branch.Value())

	// It keeps following, rather than only sampling the name once.
	m = typeInto(m, tuiFieldName, "-2")
	assert.Equal(t, "reviewer-2", m.form.branch.Value())
}

// Cycling the picker onto "create new worktree" brings the name across with
// it, so the field appears already answered instead of blank.
func TestTUISpawnWorktreePickerArrivesCarryingTheName(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m = m.openSpawnForm()
	m = typeInto(m, tuiFieldName, "packer")
	require.False(t, m.creatingWorktree(), "the form opens on (none)")
	require.NotContains(t, m.renderSpawnForm(), "Branch:", "and shows no branch field yet")

	m = m.focusSpawnField(tuiFieldWorktree).cycleChoice(1)
	assert.Equal(t, "packer", m.form.branch.Value())
	assert.Contains(t, m.renderSpawnForm(), "packer")
}

// Typing a branch of your own ends the sync: the name picker must not
// overwrite a branch the operator has chosen.
func TestTUISpawnWorktreeBranchStopsFollowingOnceTyped(t *testing.T) {
	m := spawnFormOnWorktree(t, nil)
	m = typeInto(m, tuiFieldName, "reviewer")
	m = typeInto(m, tuiFieldWorktreeBranch, "-x")
	require.False(t, m.form.branchSynced)
	assert.Equal(t, "reviewer-x", m.form.branch.Value())

	m = typeInto(m, tuiFieldName, "-2")
	assert.Equal(t, "reviewer-2", m.form.name.Value())
	assert.Equal(t, "reviewer-x", m.form.branch.Value(), "the branch is the operator's now")
}

// The branch field only exists while a new worktree is being made, so it must
// not cost a tab stop the rest of the time.
func TestTUISpawnTabOrderSkipsTheBranchUntilItIsNeeded(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m = m.openSpawnForm()
	m = m.focusSpawnField(tuiFieldDir)

	m = m.moveSpawnField(1)
	require.Equal(t, tuiFieldWorktree, m.form.field)
	m = m.moveSpawnField(1)
	assert.Equal(t, tuiFieldHarness, m.form.field, "no branch to fill in on a (none) worktree")

	// Asking for a worktree puts the branch back in the order, forwards and
	// backwards.
	m = m.focusSpawnField(tuiFieldWorktree).cycleChoice(1)
	m = m.moveSpawnField(1)
	require.Equal(t, tuiFieldWorktreeBranch, m.form.field)
	m = m.moveSpawnField(-1)
	assert.Equal(t, tuiFieldWorktree, m.form.field)
}

// A console the daemon would not treat as the human is offered no worktree at
// all — creating one runs git on the daemon's host, outside any sandbox — and
// the form says so rather than quietly dropping a field.
func TestTUISpawnWorktreeIsOperatorConsolesOnly(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = false
	m.width = 120
	m = m.openSpawnForm()
	m = m.focusSpawnField(tuiFieldDir).moveSpawnField(1)
	assert.Equal(t, tuiFieldHarness, m.form.field, "the picker is not a tab stop here")

	view := m.renderSpawnForm()
	assert.Contains(t, view, "Worktree:")
	assert.Contains(t, view, "operator consoles only")
	assert.NotContains(t, view, tuiWorktreeNew)
}

// The whole point of the field: the spawn lands in the worktree the form
// asked for, resolved before the spawn request goes out.
func TestTUISpawnResolvesTheWorktreeThenSpawnsIntoIt(t *testing.T) {
	var wtReq tuiWorktreeRequest
	var spawnReq agent.SpawnRequest
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tuiWorktreePath:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&wtReq))
			writeJSON(w, http.StatusOK, tuiWorktreeResponse{
				Path: "/repos/proj-reviewer", Branch: "reviewer", Created: true,
			})
		default:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&spawnReq))
			writeJSON(w, http.StatusOK, agent.SpawnResponse{Group: "dev", AgentID: "agt_1"})
		}
	})

	m := spawnFormOnWorktree(t, api)
	m = typeInto(m, tuiFieldName, "reviewer")
	m = typeInto(m, tuiFieldDir, "/repos/proj")

	pending, cmd := m.submitSpawn()
	require.NotNil(t, cmd)
	assert.True(t, pending.spawning)
	assert.Equal(t, tuiModeSpawn, pending.mode, "the form stays open until the worktree exists")
	assert.Contains(t, pending.notice, "Creating worktree reviewer")

	resolved, ok := cmd().(tuiWorktreeResolvedMsg)
	require.True(t, ok)
	require.NoError(t, resolved.err)
	assert.Equal(t, "/repos/proj", wtReq.Repo, "the worktree is cut in the repo the directory names")
	assert.Equal(t, "reviewer", wtReq.Branch)

	spawning, spawnCmd := pending.Update(resolved)
	got := spawning.(tuiModel)
	require.NotNil(t, spawnCmd)
	assert.Equal(t, tuiModeList, got.mode)
	assert.Contains(t, got.notice, "Created worktree /repos/proj-reviewer")

	msg, ok := spawnCmd().(tuiSpawnedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "/repos/proj-reviewer", spawnReq.Cwd, "the agent launches inside the worktree")
	assert.Equal(t, "reviewer", spawnReq.Name)

	done, _ := got.Update(msg)
	assert.Contains(t, done.(tuiModel).notice, "/repos/proj-reviewer")
}

// Naming the branch of a worktree that already exists is how this form picks
// an existing worktree — it has no list to pick one from — so the reuse is
// reported as such rather than as a creation.
func TestTUISpawnReusesAnExistingWorktree(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tuiWorktreePath {
			writeJSON(w, http.StatusOK, tuiWorktreeResponse{
				Path: "/repos/proj-feat", Branch: "feat", Created: false,
			})
			return
		}
		writeJSON(w, http.StatusOK, agent.SpawnResponse{Group: "dev", AgentID: "agt_1"})
	})

	m := spawnFormOnWorktree(t, api)
	m = typeInto(m, tuiFieldWorktreeBranch, "feat")
	pending, cmd := m.submitSpawn()
	require.NotNil(t, cmd)

	updated, _ := pending.Update(cmd())
	got := updated.(tuiModel)
	assert.Contains(t, got.notice, "Reusing the existing worktree /repos/proj-feat")
	assert.False(t, got.spawnWorktree.Created)
}

// A worktree that cannot be made is the operator's to fix in the form — a
// directory that is not a repo, a path already in the way — so the form stays
// open on the fields that produced it, and nothing is spawned.
func TestTUISpawnWorktreeFailureKeepsTheFormOpen(t *testing.T) {
	spawned := false
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tuiWorktreePath {
			writeError(w, http.StatusBadRequest, "worktree", "a new worktree needs a git repo: not a repo")
			return
		}
		spawned = true
		writeJSON(w, http.StatusOK, agent.SpawnResponse{Group: "dev", AgentID: "agt_1"})
	})

	m := spawnFormOnWorktree(t, api)
	m = typeInto(m, tuiFieldWorktreeBranch, "feat")
	pending, cmd := m.submitSpawn()
	require.NotNil(t, cmd)

	updated, next := pending.Update(cmd())
	got := updated.(tuiModel)
	assert.Nil(t, next, "a worktree that failed spawns nothing")
	assert.False(t, spawned)
	assert.Equal(t, tuiModeSpawn, got.mode)
	assert.False(t, got.spawning, "and the operator can try again")
	assert.Contains(t, got.notice, "Worktree failed")
	assert.Contains(t, got.notice, "needs a git repo")
	assert.Equal(t, "feat", got.form.branch.Value(), "what was typed is still there")
}

// An unnamed agent gets an auto-generated label the console cannot know, so a
// blank branch is asked for rather than invented.
func TestTUISpawnRefusesABlankWorktreeBranch(t *testing.T) {
	m := spawnFormOnWorktree(t, nil)
	refused, cmd := m.submitSpawn()
	assert.Nil(t, cmd)
	assert.False(t, refused.spawning)
	assert.Equal(t, tuiModeSpawn, refused.mode)
	assert.Equal(t, tuiFieldWorktreeBranch, refused.form.field, "back on the field to fix")
	assert.Contains(t, refused.notice, "branch name is required")
}

// A branch git would read as a flag never reaches git: the form refuses it
// with the field still open.
func TestTUISpawnRefusesAnUnusableBranchName(t *testing.T) {
	m := spawnFormOnWorktree(t, nil)
	m = typeInto(m, tuiFieldWorktreeBranch, "-force")
	refused, cmd := m.submitSpawn()
	assert.Nil(t, cmd)
	assert.Equal(t, tuiFieldWorktreeBranch, refused.form.field)
	assert.Contains(t, refused.notice, "Cannot use that branch name")
}

// errSpawnRefused stands in for whatever the daemon refused a spawn with; the
// notice under test is the console's own addition to it.
var errSpawnRefused = errors.New("group is archived")

// A spawn that fails after its worktree was made leaves a directory that was
// not there before. The console keeps it — it cannot tell a rejected request
// from a lost answer — and says so, rather than leaving the operator to find
// it later.
func TestTUISpawnFailureNamesTheWorktreeItKept(t *testing.T) {
	m := newTUIModel(nil)
	m.spawning = true
	m.spawnWorktree = tuiWorktreeResponse{Path: "/repos/proj-feat", Branch: "feat", Created: true}

	updated, _ := m.Update(tuiSpawnedMsg{group: "dev", err: errSpawnRefused})
	got := updated.(tuiModel)
	assert.Contains(t, got.notice, "Spawn failed")
	assert.Contains(t, got.notice, "/repos/proj-feat")
	assert.Contains(t, got.notice, "has been kept")

	// A reused worktree was already the operator's, so there is nothing to
	// warn them about.
	m.spawnWorktree.Created = false
	reused, _ := m.Update(tuiSpawnedMsg{group: "dev", err: errSpawnRefused})
	assert.NotContains(t, reused.(tuiModel).notice, "has been kept")
}

// Escaping the form while its worktree is being cut gives up the typed fields
// but does not cancel the spawn — and a second one is still refused while the
// first is in flight, so the escape cannot be used to start two.
func TestTUISpawnFormEscWhileResolvingDoesNotStartASecondSpawn(t *testing.T) {
	m := spawnFormOnWorktree(t, nil)
	m.spawning = true
	updated, _ := m.handleSpawnKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	got := updated.(tuiModel)
	require.Equal(t, tuiModeList, got.mode)

	reopened := got.openSpawnForm()
	reopened = typeInto(reopened, tuiFieldName, "second")
	refused, cmd := reopened.submitSpawn()
	assert.Nil(t, cmd)
	assert.Contains(t, refused.notice, "already in flight")
}

// ---- the endpoint ----------------------------------------------------------

// initTUIWorktreeRepo makes a real git repo with one commit on `main`, inside
// a parent temp dir — `git worktree add` cuts its default worktree as a
// sibling of the repo, so anchoring one level down keeps it inside the
// auto-cleaned tree.
func initTUIWorktreeRepo(t *testing.T) (repo, parent string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	repo = filepath.Join(parent, "proj")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, gerr := cmd.CombinedOutput(); gerr != nil {
			t.Fatalf("git %v: %v\n%s", args, gerr, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "tclaude tests")
	run("config", "commit.gpgsign", "false")
	run("commit", "-q", "--allow-empty", "-m", "init")
	run("branch", "-M", "main")
	return repo, parent
}

// postTUIWorktree drives the handler the way a console does: a JSON body and a
// peer the daemon has classified.
func postTUIWorktree(t *testing.T, body tuiWorktreeRequest, p *peer) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, tuiWorktreePath, strings.NewReader(string(raw)))
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, p))
	w := httptest.NewRecorder()
	handleTUIWorktree(w, r)
	return w
}

func operatorConsolePeer() *peer { return &peer{PID: 4242, HumanTokenValid: true} }

// The endpoint resolves one branch the way the CLI's --worktree does: create
// it the first time, reuse it the next.
func TestTUIWorktreeEndpointCreatesThenReuses(t *testing.T) {
	repo, parent := initTUIWorktreeRepo(t)

	w := postTUIWorktree(t, tuiWorktreeRequest{Repo: repo, Branch: "feat-x"}, operatorConsolePeer())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var created tuiWorktreeResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.True(t, created.Created)
	assert.Equal(t, "feat-x", created.Branch)
	assert.Equal(t, filepath.Join(parent, "proj-feat-x"), created.Path)
	info, err := os.Stat(created.Path)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	wts, err := worktree.ListWorktreesIn(repo)
	require.NoError(t, err)
	var branches []string
	for _, wt := range wts {
		branches = append(branches, wt.Branch)
	}
	assert.Contains(t, branches, "feat-x")

	again := postTUIWorktree(t, tuiWorktreeRequest{Repo: repo, Branch: "feat-x"}, operatorConsolePeer())
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	var reused tuiWorktreeResponse
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &reused))
	assert.False(t, reused.Created, "the second ask reuses rather than failing on an existing path")
	assert.Equal(t, created.Path, reused.Path)
}

// A directory that is not a git repo is a form error, not a 500.
func TestTUIWorktreeEndpointRejectsANonRepo(t *testing.T) {
	w := postTUIWorktree(t,
		tuiWorktreeRequest{Repo: t.TempDir(), Branch: "feat-x"}, operatorConsolePeer())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "needs a git repo")
}

// Creating a directory outside any sandbox is a human move. A console started
// from inside a harness pane classifies as that agent, and the agent driving
// that pane must not reach through it.
func TestTUIWorktreeEndpointRefusesAnAgentConsole(t *testing.T) {
	repo, _ := initTUIWorktreeRepo(t)
	w := postTUIWorktree(t, tuiWorktreeRequest{Repo: repo, Branch: "feat-x"},
		&peer{PID: 4242, ConvID: "conv-1", HasClaudeAncestor: true})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "operator-console operation")

	_, err := os.Stat(filepath.Join(filepath.Dir(repo), "proj-feat-x"))
	assert.True(t, os.IsNotExist(err), "and nothing was created")
}

// The route is the console's, not the agent API's: it must never appear on
// the mux the Unix socket serves.
func TestTUIWorktreeRouteIsConsoleOnly(t *testing.T) {
	call := func(h http.Handler) int {
		r := httptest.NewRequest(http.MethodPost, tuiWorktreePath, strings.NewReader("{}"))
		r = r.WithContext(context.WithValue(r.Context(), peerKey{}, operatorConsolePeer()))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	// A blank branch is the handler's own 400 — proof it ran at all.
	assert.Equal(t, http.StatusBadRequest, call(buildTUIConsoleMux()))
	assert.Equal(t, http.StatusNotFound, call(buildMux()),
		"agents reach worktrees through the `tclaude worktree` CLI, not this route")
}

func TestValidateTUIWorktreeBranch(t *testing.T) {
	ok := []string{"reviewer", "TCL-123_fix", "team/feat.x", "a"}
	for _, branch := range ok {
		assert.NoErrorf(t, validateTUIWorktreeBranch(branch), "branch %q", branch)
	}
	bad := map[string]string{
		"":              "required",
		"-force":        "may not start",
		"/leading":      "may not start",
		".hidden":       "may not start",
		"trailing/":     "may not end",
		"trailing.":     "may not end",
		"up/../out":     "may not contain",
		"double//slash": "may not contain",
		"has space":     "only letters",
		"semi;colon":    "only letters",
		strings.Repeat("x", tuiMaxWorktreeBranchLen+1): "too long",
	}
	for branch, want := range bad {
		err := validateTUIWorktreeBranch(branch)
		require.Errorf(t, err, "branch %q should be refused", branch)
		assert.Containsf(t, err.Error(), want, "branch %q", branch)
	}
}
