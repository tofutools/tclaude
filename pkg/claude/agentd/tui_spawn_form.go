package agentd

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// tuiSpawnForm is the "new agent" prompt. Group, profile and harness are
// cycled choices; the rest are text inputs.
type tuiSpawnForm struct {
	field int

	groupNames []string
	groupIdx   int

	profileNames []string
	profileIdx   int

	harnessNames []string
	harnessIdx   int

	worktreeNames []string
	worktreeIdx   int

	name   textinput.Model
	dir    textinput.Model
	branch textinput.Model
	brief  textinput.Model
	model  textinput.Model
	effort textinput.Model
	// branchSynced is the worktree branch's "still following the name" state:
	// while it holds, every keystroke in Name is copied into the branch field,
	// so the common case (one agent, its own worktree, named after it) needs
	// nothing typed twice. Typing in the branch field itself ends the sync —
	// after that the branch is the operator's and the name picker leaves it
	// alone, the same contract the directory prefill has with the group picker.
	branchSynced bool
	// gen identifies this form among the ones this console has opened, so a
	// worktree resolution that lands after the operator has escaped and
	// reopened the prompt can tell "the form that asked" from "the form on
	// screen" — and leave the second one, with everything typed into it,
	// alone. Assigned by openSpawnForm from the model's counter.
	gen uint64
	// dirPrefill is the value prefillDir last wrote into dir — the selected
	// group's default directory. It is what tells an untouched field from one
	// the operator has typed in, so changing the group can follow the first
	// and must leave the second alone.
	dirPrefill string
	// namePrefill is the same idea one field up: the value applyProfile last
	// wrote into name, which is the selected profile's agent_name. Changing the
	// profile follows an untouched name field and leaves a typed one alone.
	namePrefill string
	// worktreeTouched records that the operator has cycled the worktree picker
	// themselves. Until they do, a profile's sync_worktree toggle sets it; after
	// they do, the picker is theirs — the same contract dirPrefill and
	// namePrefill give the two text fields.
	worktreeTouched bool
	// dirSuggestions holds the ambiguous Tab-completion candidates for dir,
	// listed under the field until the next keystroke.
	dirSuggestions []string
}

// tuiHarnessOptions lists the harnesses a spawn may name, led by the
// "(default)" sentinel that pins nothing.
func tuiHarnessOptions() []string {
	out := []string{tuiHarnessDefault}
	for _, name := range harness.Names() {
		if h, ok := harness.Get(name); ok && h.Spawn != nil && h.Spawnable() {
			out = append(out, name)
		}
	}
	return out
}

// tuiProfileOptions lists the spawn profiles the form may name, led by the
// "(default)" sentinel that names none. A disabled profile is left out: the
// daemon refuses a spawn that selects one, and nothing in this console can
// re-enable it.
func tuiProfileOptions(profiles []tuiProfileRow) []string {
	out := []string{tuiProfileDefault}
	for _, p := range profiles {
		if p.Name != "" && !p.disabled() {
			out = append(out, p.Name)
		}
	}
	return out
}

func newTUITextInput(prompt string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = prompt
	ti.SetWidth(60)
	// The field takes the terminal's own paste (see deliverPaste) but not
	// textinput's ctrl+v, which reads the host's system clipboard. This
	// console can be agent-class — a pane a harness drives through tmux
	// send-keys, the same reason Tab completion is gated in completingDir —
	// and a keystroke that pulls whatever the human last copied into a
	// visible field is a wider door than that caller should have. A terminal
	// paste carries only what the pasting side already had.
	ti.KeyMap.Paste.SetEnabled(false)
	return ti
}

// openSpawnForm builds a fresh "new agent" prompt over the current group and
// profile lists.
//
// Both cycled launch fields start on "(default)", which is the only setting
// under which the daemon's resolution chain can do its job. The harness picker
// used to open on the default harness by name instead, which reads the same
// but is an explicit pin on the wire — and an explicit harness outranks every
// profile tier, so a profile that selects Codex would have been overruled by a
// field the operator never touched.
func (m tuiModel) openSpawnForm() tuiModel {
	m.spawnFormGen++
	form := tuiSpawnForm{
		gen:           m.spawnFormGen,
		harnessNames:  tuiHarnessOptions(),
		profileNames:  tuiProfileOptions(m.profiles),
		worktreeNames: tuiWorktreeOptions(),
		name:          newTUITextInput("  Name:      "),
		dir:           newTUITextInput("  Directory: "),
		branch:        newTUITextInput("  Branch:    "),
		brief:         newTUITextInput("  Brief:     "),
		model:         newTUITextInput("  Model:     "),
		effort:        newTUITextInput("  Effort:    "),
		// The branch follows the name until the operator says otherwise, so
		// picking "create new worktree" on a named agent needs nothing else
		// typed. Nothing is created until they pick it: the form opens on
		// "(none)", which is the spawn every earlier version of this form made.
		branchSynced: true,
	}
	for _, g := range m.groups {
		form.groupNames = append(form.groupNames, g.Name)
	}
	m.form = form
	m.mode = tuiModeSpawn
	return m.prefillDir()
}

// groupDefaultDir is the group's configured default working directory, ending
// in a separator, or "" when it has none (or when the daemon does not serve
// this console the path — see tuiGroupRow).
//
// The trailing separator is the point of the prefill: it makes the group's
// directory a starting point rather than an answer, so the operator types the
// subdirectory they want straight onto it and Tab lists what is in there.
func (m tuiModel) groupDefaultDir(name string) string {
	for _, g := range m.groups {
		if g.Name != name {
			continue
		}
		if dir := strings.TrimSpace(g.DefaultCwd); dir != "" {
			return strings.TrimSuffix(dir, "/") + "/"
		}
		return ""
	}
	return ""
}

// prefillDir points the directory field at the selected group's default
// directory, which is where that group's agents are launched anyway — so the
// common "somewhere under the group's directory" spawn is a subdirectory name
// away instead of a full path retyped.
//
// It only ever writes a field the operator has not touched. Once they have
// typed their own path, cycling the group picker leaves it exactly as it is:
// the alternative silently discards a path they may have Tab-completed their
// way to.
func (m tuiModel) prefillDir() tuiModel {
	// A cleared field counts as untouched: blank already means "the group's
	// default directory", so filling in the new group's own is the same
	// launch either way — and it puts the path back where it can be extended.
	if v := m.form.dir.Value(); v != "" && v != m.form.dirPrefill {
		return m
	}
	m.form.dirPrefill = m.groupDefaultDir(m.selectedGroup())
	m.form.dir.SetValue(m.form.dirPrefill)
	m.form.dir.CursorEnd()
	return m
}

func (m tuiModel) selectedGroup() string {
	if m.form.groupIdx < 0 || m.form.groupIdx >= len(m.form.groupNames) {
		return ""
	}
	return m.form.groupNames[m.form.groupIdx]
}

// selectedProfile is the profile name the request should carry, empty for the
// "(default)" sentinel — which is what lets the group's and the global default
// profile apply as they do for every other spawn surface.
func (m tuiModel) selectedProfile() string {
	if m.form.profileIdx < 0 || m.form.profileIdx >= len(m.form.profileNames) {
		return ""
	}
	if name := m.form.profileNames[m.form.profileIdx]; name != tuiProfileDefault {
		return name
	}
	return ""
}

// selectedProfileRow is the listing row behind the profile picker's current
// choice, absent for the "(default)" sentinel and for a name the last refresh
// has since dropped.
func (m tuiModel) selectedProfileRow() (tuiProfileRow, bool) {
	name := m.selectedProfile()
	if name == "" {
		return tuiProfileRow{}, false
	}
	for _, p := range m.profiles {
		if p.Name == name {
			return p, true
		}
	}
	return tuiProfileRow{}, false
}

// applyProfile pulls into the form the two spawn-profile fields this form has
// to apply itself: the agent name, and whether the agent is spawned into a
// worktree of its own. Everything else a profile carries — harness, model,
// approval posture, role, descr, the brief, group context, the owner flag,
// permission overrides — the daemon resolves from the profile NAME the request
// posts, so the form neither reads nor shows it.
//
// Both follow the directory prefill's contract with the group picker: a field
// the operator has already made theirs is left alone, and an untouched one
// follows the profile.
func (m tuiModel) applyProfile() tuiModel {
	// "(default)" names no profile, so it takes back the name the last one
	// prefilled rather than stranding it on a form that no longer explains it.
	// The worktree picker is left where it is: naming no profile says nothing
	// about worktrees, and silently disarming one would lose a branch the
	// operator may already have typed.
	prof, ok := m.selectedProfileRow()
	if v := m.form.name.Value(); v == "" || v == m.form.namePrefill {
		m.form.namePrefill = strings.TrimSpace(prof.AgentName)
		m.form.name.SetValue(m.form.namePrefill)
		m.form.name.CursorEnd()
	}
	if !ok {
		return m.syncWorktreeBranch()
	}
	// The picker only exists for a console allowed to cut worktrees, so a
	// profile cannot arm it on one that is not (see canCreateWorktree).
	if sync, set := prof.syncsWorktree(); set && m.canCreateWorktree() && !m.form.worktreeTouched {
		want := tuiWorktreeNone
		if sync {
			want = tuiWorktreeNew
		}
		for i, name := range m.form.worktreeNames {
			if name == want {
				m.form.worktreeIdx = i
				break
			}
		}
	}
	return m.syncWorktreeBranch()
}

func (m tuiModel) selectedHarness() string {
	if m.form.harnessIdx < 0 || m.form.harnessIdx >= len(m.form.harnessNames) {
		return ""
	}
	if name := m.form.harnessNames[m.form.harnessIdx]; name != tuiHarnessDefault {
		return name
	}
	return ""
}

// creatingWorktree reports whether the form's worktree picker is asking for a
// new worktree rather than a plain launch directory.
func (m tuiModel) creatingWorktree() bool {
	return m.form.worktreeIdx >= 0 && m.form.worktreeIdx < len(m.form.worktreeNames) &&
		m.form.worktreeNames[m.form.worktreeIdx] == tuiWorktreeNew
}

// canCreateWorktree gates the worktree fields on the console being the
// operator: resolving a worktree runs git as the daemon process — the human's
// own filesystem, outside any agent sandbox — and it CREATES a directory of
// the caller's choosing. A console started from inside a harness pane is
// agent-class and drivable by that agent through tmux send-keys, so it is
// offered the field no more than the daemon would honour it (see
// handleTUIWorktree, which refuses the same caller).
//
// Unlike directory completion and the shell form, that is the WHOLE gate:
// those two also need the console to share the daemon's host and terminal
// (completeLocalDir / startLocalShell), because they read or drive this
// machine. A worktree is cut on the daemon's host in either case, so a remote
// operator console gets the field too.
func (m tuiModel) canCreateWorktree() bool { return m.operator }

// spawnFieldEnabled reports whether a field takes part in this form at all.
// The two disabled shapes are a console that may not create worktrees, and the
// branch field of a form that is not creating one — a field with nothing to
// say should not cost a tab stop.
func (m tuiModel) spawnFieldEnabled(field int) bool {
	switch field {
	case tuiFieldWorktree:
		return m.canCreateWorktree()
	case tuiFieldWorktreeBranch:
		return m.canCreateWorktree() && m.creatingWorktree()
	default:
		return true
	}
}

// moveSpawnField shifts focus through the form, wrapping, and moves the
// text-input focus with it — the group, profile, worktree and harness fields
// have no input model behind them, so all of the inputs blur for those.
// Disabled fields are stepped over rather than landed on.
func (m tuiModel) moveSpawnField(delta int) tuiModel {
	step := 1
	if delta < 0 {
		step = -1
	}
	// Bounded by the field count: every field being disabled is impossible
	// (the text inputs always are), but a loop over a form must not depend on
	// that to terminate.
	for range tuiSpawnFieldCount {
		m.form.field = ((m.form.field+step)%tuiSpawnFieldCount + tuiSpawnFieldCount) % tuiSpawnFieldCount
		if m.spawnFieldEnabled(m.form.field) {
			break
		}
	}
	return m.focusSpawnField(m.form.field)
}

// focusSpawnField puts the form on one field and moves the text-input focus
// with it, without stepping over anything — used by moveSpawnField and by a
// refused submit, which sends the operator back to the field to fix.
func (m tuiModel) focusSpawnField(field int) tuiModel {
	m.form.field = field
	m.form.name.Blur()
	m.form.dir.Blur()
	m.form.branch.Blur()
	m.form.brief.Blur()
	m.form.model.Blur()
	m.form.effort.Blur()
	switch field {
	case tuiFieldName:
		m.form.name.Focus()
	case tuiFieldDir:
		m.form.dir.Focus()
	case tuiFieldWorktreeBranch:
		m.form.branch.Focus()
	case tuiFieldModel:
		m.form.model.Focus()
	case tuiFieldEffort:
		m.form.effort.Focus()
	case tuiFieldBrief:
		m.form.brief.Focus()
	}
	return m
}

// cycleChoice steps whichever of the choice fields is focused.
func (m tuiModel) cycleChoice(delta int) tuiModel {
	switch m.form.field {
	case tuiFieldGroup:
		if n := len(m.form.groupNames); n > 0 {
			m.form.groupIdx = ((m.form.groupIdx+delta)%n + n) % n
			// The directory follows the group it belongs to, until the
			// operator types one of their own.
			m = m.prefillDir()
		}
	case tuiFieldProfile:
		if n := len(m.form.profileNames); n > 0 {
			m.form.profileIdx = ((m.form.profileIdx+delta)%n + n) % n
			// The two profile fields this form has to apply itself follow the
			// profile they came from, until the operator overrides them.
			m = m.applyProfile()
		}
	case tuiFieldHarness:
		if n := len(m.form.harnessNames); n > 0 {
			m.form.harnessIdx = ((m.form.harnessIdx+delta)%n + n) % n
		}
	case tuiFieldWorktree:
		if n := len(m.form.worktreeNames); n > 0 {
			m.form.worktreeIdx = ((m.form.worktreeIdx+delta)%n + n) % n
			m.form.worktreeTouched = true
			// Turning the picker to "create new worktree" is what makes the
			// branch field appear, so it arrives already carrying the name —
			// the sync's whole point is that the operator names the agent once.
			m = m.syncWorktreeBranch()
		}
	}
	return m
}

// syncWorktreeBranch copies the agent's name into the branch field while the
// branch is still following it. A blank name leaves a blank branch, which the
// submit refuses by name rather than guessing one: an unnamed agent gets an
// auto-generated label the console cannot know in advance, and cutting a
// branch called something the operator never saw is worse than asking.
//
// The leading characters a branch may not start with are dropped on the way
// across (see validateTUIWorktreeBranch): the spawn-name charset allows a
// leading "-", so a perfectly good agent name would otherwise sync into a
// branch the form then refuses — on a field the operator never typed in. The
// trimmed value is on screen, so nothing is renamed behind their back.
func (m tuiModel) syncWorktreeBranch() tuiModel {
	if !m.form.branchSynced {
		return m
	}
	m.form.branch.SetValue(strings.TrimLeft(strings.TrimSpace(m.form.name.Value()), "-./"))
	m.form.branch.CursorEnd()
	return m
}

// tuiIsChoiceField reports whether field is one of the cycled pickers, which
// is what makes ←/→ (and space) change a value rather than reach a text input.
func tuiIsChoiceField(field int) bool {
	switch field {
	case tuiFieldGroup, tuiFieldProfile, tuiFieldHarness, tuiFieldWorktree:
		return true
	default:
		return false
	}
}

// completingDir reports whether a Tab should complete a path rather than
// move to the next field: this transport shares the daemon filesystem, the
// caller is an operator, the directory field is focused, and there is
// something to complete.
//
// The operator check is the same gate attachSelected uses, for the same
// reason. Completion reads the filesystem as the process agentd runs in —
// the human's own, outside any agent sandbox — and a console started from
// inside a harness pane is agent-class and drivable by that agent through
// tmux send-keys. Ungated, Tab would hand it bulk directory listings from
// outside its sandbox. A failed spawn already leaks whether one guessed
// path exists, but that is a per-path oracle; enumerating names the agent
// could not have guessed is a different thing, so the console does not
// offer it to a caller the daemon would not treat as the human.
func (m tuiModel) completingDir() bool {
	if !m.operator || !m.capabilities.completeLocalDir || m.form.field != tuiFieldDir {
		return false
	}
	// Only a path the operator has typed into is completed. An untouched
	// field — blank, or the group's own directory as the form filled it in —
	// keeps Tab's ordinary next-field job, which is the only way to tab past
	// Directory at all. It would not be a harmless completion either:
	// CompleteDirPath resolves a directory with exactly one child straight
	// into that child, so a Tab meant as "next field" would silently spawn
	// the agent somewhere the operator never chose.
	value := m.form.dir.Value()
	return value != "" && value != m.form.dirPrefill
}

// completeDir runs one round of bash-like directory completion on the
// directory field — the same helper the `session watch` new-session prompt
// uses, so both forms complete paths identically. An unambiguous match is
// completed through its trailing "/" (Tab again walks further down); an
// ambiguous one extends as far as it can and leaves the candidates for the
// form to list.
func (m tuiModel) completeDir() tuiModel {
	completed, candidates := clcommon.CompleteDirPath(m.form.dir.Value())
	m.form.dir.SetValue(completed)
	m.form.dir.CursorEnd()
	m.form.dirSuggestions = candidates
	return m
}

func (m tuiModel) updateFocusedInput(msg tea.Msg) (tuiModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.form.field {
	case tuiFieldName:
		m.form.name, cmd = m.form.name.Update(msg)
		// The branch follows the name as it is typed, so the operator sees the
		// branch they are about to get rather than discovering it at submit.
		m = m.syncWorktreeBranch()
	case tuiFieldDir:
		m.form.dir, cmd = m.form.dir.Update(msg)
	case tuiFieldWorktreeBranch:
		before := m.form.branch.Value()
		m.form.branch, cmd = m.form.branch.Update(msg)
		// Any edit here ends the sync — including clearing the field, which is
		// how an operator says "not this name" rather than "no opinion". Keys
		// that change nothing (cursor moves) leave the sync alone.
		if m.form.branch.Value() != before {
			m.form.branchSynced = false
		}
	case tuiFieldModel:
		m.form.model, cmd = m.form.model.Update(msg)
	case tuiFieldEffort:
		m.form.effort, cmd = m.form.effort.Update(msg)
	case tuiFieldBrief:
		m.form.brief, cmd = m.form.brief.Update(msg)
	}
	return m, cmd
}

// submitSpawn turns the form into a spawn request. A blank directory is left
// blank on purpose: the daemon then falls back to the group's default_cwd,
// which is what the operator configured it for.
func (m tuiModel) submitSpawn() (tuiModel, tea.Cmd) {
	group := m.selectedGroup()
	if group == "" {
		// An empty picker means one of two very different things, and saying
		// "no groups" to someone whose groups simply have not arrived yet
		// sends them off to create a duplicate.
		if m.lastRefresh.IsZero() {
			m.notice = "Group list has not loaded yet — press r to refresh, then try again."
		} else {
			m.notice = "No groups yet — create one first: tclaude agent groups create <name>"
		}
		return m, nil
	}
	if m.spawning {
		m.notice = "A spawn is already in flight."
		return m, nil
	}
	req := agent.SpawnRequest{
		Cwd:            strings.TrimSpace(m.form.dir.Value()),
		Profile:        m.selectedProfile(),
		Harness:        m.selectedHarness(),
		Model:          strings.TrimSpace(m.form.model.Value()),
		Effort:         strings.TrimSpace(m.form.effort.Value()),
		InitialMessage: strings.TrimSpace(m.form.brief.Value()),
	}
	// The name is STATED, not merely set: this form has a box for it and fills
	// that box from the selected profile, so an emptied box is the operator
	// saying "not that name" — not a silence for the profile to answer again.
	//
	// The brief is deliberately NOT stated. This form never prefills it (a
	// profile's brief may be many lines, and this is a one-line input that would
	// mangle them), so there is nothing here for the operator to have cleared:
	// an empty box is a real silence, and the profile's brief fills it as the
	// task default it is. briefHint says so on screen.
	req.StateName(strings.TrimSpace(m.form.name.Value()))
	m.spawnWorktree = tuiWorktreeResponse{}
	if m.canCreateWorktree() && m.creatingWorktree() {
		branch := strings.TrimSpace(m.form.branch.Value())
		if err := validateTUIWorktreeBranch(branch); err != nil {
			// The daemon refuses the same names for the same reasons; checking
			// here keeps the form open on the field the operator has to fix,
			// instead of closing it and reporting the refusal over the listing.
			m.notice = "Cannot use that branch name: " + err.Error()
			return m.focusSpawnField(tuiFieldWorktreeBranch), nil
		}
		// The form stays open until the worktree exists. Everything that can go
		// wrong from here — not a git repo, a name git will not take, a
		// directory already in the way — is answered by editing a field, and
		// closing the form would throw away the brief along with it.
		m.spawning = true
		m.notice = "Creating worktree " + branch + "…"
		return m, tuiResolveWorktreeCmd(m.api,
			tuiWorktreeRequest{Repo: m.worktreeRepoDir(), Branch: branch}, group, m.form.gen, req)
	}
	m.mode = tuiModeList
	m.spawning = true
	m.notice = "Spawning in group " + group + "…"
	return m, tuiSpawnCmd(m.api, group, req)
}

// worktreeRepoDir is the directory the new worktree's repo is looked up from:
// the directory field, falling back to the group's default — which is where a
// blank field spawns anyway — and then to nothing, which the daemon resolves
// as its own working directory, the last step of that same fallback chain.
//
// The agent then launches inside the resolved worktree rather than in this
// directory, which is what `tclaude agent spawn --worktree` does too.
func (m tuiModel) worktreeRepoDir() string {
	if dir := strings.TrimSpace(m.form.dir.Value()); dir != "" {
		return dir
	}
	return strings.TrimSpace(m.groupDefaultDir(m.selectedGroup()))
}
