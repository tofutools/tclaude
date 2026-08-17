package session

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The harness-config read-only floor.
//
// Under tclaude-layer the launch contract binds the harness's own state root
// READ-WRITE, because that is where the harness keeps the state it must write:
// transcripts, project records, todo lists, history, account/onboarding data.
// The same directory also holds the harness's POLICY and PERSISTENT-CODE
// surface — its settings file, its hook/skill/agent/command directories, its
// memory file — and those are a different kind of thing.
//
// A confined agent that writes them escalates out of its own sandbox by
// waiting:
//
//   - Claude Code's ~/.claude/settings.json carries the block
//     `tclaude setup --install-sandbox-hardening` installs, which is what walls
//     ~/.tclaude/data (operator token, remote-access CA) and ~/.claude/sessions
//     (identity forgery) off from every harness-builtin launch and from the
//     human's own ambient runs. Strip it and that wall is gone.
//   - Codex's tclaude-agent.config.toml IS the managed profile that provides
//     Codex's harness-builtin confinement. Rewrite it and the next
//     harness-builtin Codex launch comes up unconfined.
//   - A hook, skill, agent, or command dropped in any of these directories
//     executes in the human's NEXT harness session, outside any sandbox.
//
// So these paths are floored read-only by default. Claude Code's own sandbox
// already deny-writes almost exactly this set for its Bash tool; without the
// floor tclaude's outer wall is weaker than the harness's own default, which
// is backwards.
//
// Two escape hatches, in increasing bluntness:
//
//   - An operator profile that authors an explicit write grant AT or BELOW one
//     floor path drops that single entry. Most-specific-wins, and the operator
//     has to name the surface, so a broad `~` or `~/.claude` write does not
//     quietly undo the floor.
//   - `harness_config: "write"` on any profile in the chain turns the whole
//     floor off, restoring the pre-floor posture.
//
// The floor covers only what tclaude itself enforces: tclaude-layer and
// stacked. Under harness-builtin the harness's own policy governs, and under
// resource-only/off nothing is enforced by design.
//
// Note the asymmetry with Claude Code's Bash-only deny list: a bubblewrap
// read-only bind blocks EVERYONE inside the wall, the harness process
// included. In-pane writes to a floored file therefore fail — Claude Code's
// `/config` and user-scope `/permissions`, Codex's `/model` persistence,
// Copilot's trust-folder record. That is the cost, and it is why the opt-out
// exists.

// harnessConfigFloorEntry is one floored path. Dir entries are materialized as
// directories so an agent cannot create a missing surface and write into it;
// file entries are materialized as empty files for the same reason (empty is
// semantically identical to absent for every file in the catalog).
type harnessConfigFloorEntry struct {
	Path  string
	IsDir bool
}

// harnessConfigFloorCatalog is the per-harness closed list, spelled relative to
// the harness state root except where an absolute path is returned. Adding to
// it narrows what an agent may write, so entries need to be surfaces the
// harness reads as policy or executes as code — not merely state the operator
// would rather keep tidy.
func harnessConfigFloorCatalog(
	harnessName string,
	stateRoot string,
) ([]harnessConfigFloorEntry, error) {
	stateRoot = filepath.Clean(strings.TrimSpace(stateRoot))
	if stateRoot == "." || !filepath.IsAbs(stateRoot) {
		return nil, fmt.Errorf(
			"harness config floor needs an absolute state root, got %q", stateRoot)
	}
	var dirs, files []string
	under := func(names ...string) []string {
		out := make([]string, 0, len(names))
		for _, name := range names {
			out = append(out, filepath.Join(stateRoot, name))
		}
		return out
	}
	switch harnessName {
	case harness.DefaultName:
		// CLAUDE_CONFIG_DIR is pinned to the state root for every tclaude
		// launch (see ApplyClaudeConfigDirEnv), so the whole Claude Code config
		// surface really does live here.
		//
		// `local` holds the local `claude` launcher — direct code execution as
		// the human, so it belongs here even though it costs in-sandbox
		// auto-update.
		//
		// NOT floored, deliberately: `.claude.json`, `projects/`, `todos/`,
		// `history.jsonl`, `shell-snapshots/`, `statsig/`, `file-history/`.
		// Claude Code writes all of them continuously, so flooring them would
		// break the launch outright. `.claude.json` is the uncomfortable one —
		// it carries `mcpServers`, i.e. commands Claude Code execs — and it is
		// a residual hole this floor does NOT close. See "Honestly residual
		// holes" in docs/sandboxing.md.
		dirs = under("hooks", "skills", "agents", "commands", "output-styles",
			"plugins", "workflows", "routines", "rules", "local",
			"cowork_plugins")
		files = under("settings.json", "settings.local.json", "CLAUDE.md",
			"keybindings.json")
	case harness.CodexName:
		dirs = under("hooks", "prompts")
		files = under("config.toml", "hooks.json", "AGENTS.md")
		// The setup-managed profile IS Codex's harness-builtin confinement.
		// Per-launch tclaude-agent-<hex>.config.toml siblings are not floored:
		// they are generated fresh per launch and swept afterwards, so they
		// carry no authority into a future launch the way this one does.
		files = append(files, filepath.Join(
			stateRoot, harness.CodexAgentProfile+".config.toml"))
	case harness.CopilotName:
		dirs = under("hooks")
		// config.json outranks settings.json for Copilot's sandbox key, so both
		// have to be floored or the weaker one decides.
		files = under(harness.CopilotSettingsFileName, "config.json",
			"mcp-config.json")
	case harness.OpenCodeName:
		// Nothing. OpenCode's config surface is ALREADY floored by its own
		// state management: agentd's layout binds the whole ambient config
		// tree read-only (daemon-final) in both legacy-shared and private
		// modes, and in private mode the sandboxed executor reads
		// <stateRoot>/config/opencode instead of the ambient root entirely
		// (pkg/claude/agentd/opencode_state_unix.go). A catalog here would be
		// redundant, would aim at the wrong root under private state, and
		// would materialize junk in the operator's real ~/.config/opencode.
	default:
		return nil, fmt.Errorf(
			"harness config floor has no catalog for harness %q", harnessName)
	}
	entries := make([]harnessConfigFloorEntry, 0, len(dirs)+len(files))
	for _, path := range dirs {
		entries = append(entries, harnessConfigFloorEntry{Path: path, IsDir: true})
	}
	for _, path := range files {
		entries = append(entries, harnessConfigFloorEntry{Path: path})
	}
	out := make([]harnessConfigFloorEntry, 0, len(entries))
	for _, entry := range entries {
		canonical, floorable, err := canonicalHarnessConfigFloorPath(entry.Path)
		if err != nil {
			return nil, fmt.Errorf(
				"resolve harness config floor path %q: %w", entry.Path, err)
		}
		if !floorable {
			// Disclosed, not silent, and not launch-breaking. See the doc
			// comment on canonicalHarnessConfigFloorPath for why a symlinked
			// entry cannot be floored faithfully.
			slog.Warn("harness config floor skips a symlinked path; it stays writable",
				"path", canonical, "harness", harnessName, "module", "sandbox")
			continue
		}
		entry.Path = canonical
		out = append(out, entry)
	}
	return out, nil
}

// canonicalHarnessConfigFloorPath canonicalizes a floor entry through its
// PARENT only, deliberately leaving the final component unresolved, and
// reports whether the entry can be floored at all.
//
// Resolving the final component would defeat the floor for a symlinked entry.
// Dotfile managers commonly point ~/.claude/skills at a repo. Bind the
// RESOLVED target read-only and the name ~/.claude/skills is still an ordinary
// symlink dentry inside the writable state root: the agent deletes it, mkdirs
// a real directory in its place, and the human's next unsandboxed session
// loads whatever it wants. Flooring the literal name instead makes it a
// mountpoint, which cannot be unlinked from inside — the same property that
// protects every non-symlinked entry.
//
// A final component that IS a symlink therefore cannot be floored faithfully:
// binding over it resolves right back to the target. Rather than refuse the
// launch (a hard regression for every dotfiles-managed harness config) or
// pretend (a silent hole), such an entry is skipped with a warning and named
// as a known limitation in docs/sandboxing.md.
//
// Going through the parent also handles the ordinary case that
// canonicalTclaudeLayerStatePath rejects outright: an existing settings FILE
// is not a directory, and that is normal rather than an error.
func canonicalHarnessConfigFloorPath(path string) (string, bool, error) {
	parent, err := canonicalTclaudeLayerStatePath(filepath.Dir(path))
	if err != nil {
		return "", false, err
	}
	resolved := filepath.Join(parent, filepath.Base(path))
	info, err := os.Lstat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing there yet: materialization will create a real node.
			return resolved, true, nil
		}
		return "", false, err
	}
	return resolved, info.Mode()&os.ModeSymlink == 0, nil
}

// harnessConfigFloorPaths resolves the floor for one launch and drops the
// entries an operator explicitly reopened. It returns nil when the composed
// posture opted the whole floor out, which is also what makes the frozen
// contract self-describing: an empty list means "no floor was applied", never
// "the floor was applied and happened to be empty".
func harnessConfigFloorPaths(
	harnessName string,
	stateRoot string,
	access sandboxpolicy.HarnessConfigAccess,
	profileFilesystem []sandboxpolicy.FilesystemGrant,
) ([]harnessConfigFloorEntry, error) {
	if !sandboxpolicy.HarnessConfigFloorApplies(access) {
		return nil, nil
	}
	entries, err := harnessConfigFloorCatalog(harnessName, stateRoot)
	if err != nil {
		return nil, err
	}
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return nil, fmt.Errorf("resolve protected paths for harness config floor: %w", err)
	}
	out := make([]harnessConfigFloorEntry, 0, len(entries))
	for _, entry := range entries {
		skip := false
		for _, protected := range protectedRoots {
			if sandboxpolicy.GuardContainsOrEqual(protected, entry.Path) {
				// The protected wall already hides this more completely than a
				// read-only bind would, and a contract path that intersects it
				// is refused downstream. Nothing to add.
				skip = true
				break
			}
		}
		if skip || harnessConfigFloorReopened(entry.Path, profileFilesystem) {
			continue
		}
		// A profile deny is STRICTER than the floor, so the floor has nothing
		// to add and must not materialize a host path the operator denied.
		if access, covered := sandboxpolicy.EffectiveAccessAt(
			profileFilesystem, entry.Path); covered && access == sandboxpolicy.AccessDeny {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// harnessConfigFloorReopened reports whether the operator explicitly took
// responsibility for one floored path, which drops that entry from the floor.
//
// Only a write grant at EXACTLY this path counts, and the two exclusions on
// either side are what make the axis behave:
//
//   - An ANCESTOR grant does not count. A blanket `~` or state-root write is
//     the ordinary shape of an unrelated profile, and letting it disable the
//     floor would make the default meaningless for exactly the profiles that
//     most need one.
//   - A DESCENDANT grant does not count either. `~/.claude/hooks/mine` asks
//     for one path, not for the directory, so the floor stays on the rest of
//     `~/.claude/hooks` and the narrower RW bind lands on top of it — the
//     mount plan renders ancestors first. tclaudeLayerHarnessStateRule's
//     AllowNarrowerWrite is what keeps that from reading as a contract
//     conflict and refusing the launch.
func harnessConfigFloorReopened(
	path string,
	profileFilesystem []sandboxpolicy.FilesystemGrant,
) bool {
	path = filepath.Clean(path)
	for _, grant := range profileFilesystem {
		if grant.Access != sandboxpolicy.AccessWrite {
			continue
		}
		if filepath.Clean(grant.Path) == path {
			return true
		}
	}
	return false
}

// harnessConfigFloorSeed is the body a materialized file entry gets. An empty
// file is NOT equivalent to an absent one for JSON: every reader in this repo
// (session.InstallHooks, agentd.writeUserDefaultModel, the dashboard's global
// config reader) treats a missing file as "start from {}" but hands an
// existing empty one to json.Unmarshal, which fails. Claude Code itself
// reports an unparseable user settings file the same way. So JSON entries are
// seeded with an empty object; TOML and Markdown are genuinely equivalent to
// absent when empty.
func harnessConfigFloorSeed(path string) []byte {
	switch filepath.Ext(path) {
	case ".json", ".jsonc":
		return []byte("{}\n")
	default:
		return nil
	}
}

// prepareHarnessConfigFloor materializes the floored paths before the outer
// layer starts. Materializing rather than skipping missing paths is the whole
// point: an absent ~/.claude/hooks under a writable state root is a directory
// the agent can simply create and then write into, so a floor that only
// covered what already existed would protect the hosts that need it least.
//
// What is written is chosen to be indistinguishable from absent for the
// harness and for tclaude's own readers: an empty directory, or the seed body
// above.
func prepareHarnessConfigFloor(paths []string, dirs map[string]bool) error {
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return fmt.Errorf("resolve protected paths for harness config floor: %w", err)
	}
	for _, path := range paths {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || !filepath.IsAbs(path) {
			return fmt.Errorf("harness config floor path %q is not absolute", path)
		}
		for _, protected := range protectedRoots {
			if sandboxpolicy.GuardContainsOrEqual(protected, path) {
				return fmt.Errorf(
					"harness config floor path %q is at or below protected root %q",
					path, protected)
			}
		}
		if dirs[path] {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("prepare harness config floor %q: %w", path, err)
			}
			// Lstat, not Stat: a symlinked entry is skipped at derivation
			// time, so reaching one here means it was swapped in the window
			// since. Refuse rather than bind through it.
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("inspect harness config floor %q: %w", path, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf(
					"harness config floor path %q became a symlink; refusing to bind through it", path)
			}
			if !info.IsDir() {
				return fmt.Errorf("harness config floor path %q is not a directory", path)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("prepare harness config floor parent of %q: %w", path, err)
		}
		// O_EXCL, and a pre-existing path is left exactly as it is: this must
		// never truncate an operator's real settings file, and losing a race
		// with the harness or another launch is not an error.
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if _, writeErr := file.Write(harnessConfigFloorSeed(path)); writeErr != nil {
				_ = file.Close()
				return fmt.Errorf("prepare harness config floor %q: %w", path, writeErr)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("prepare harness config floor %q: %w", path, err)
			}
			continue
		}
		if !os.IsExist(err) {
			return fmt.Errorf("prepare harness config floor %q: %w", path, err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect harness config floor %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf(
				"harness config floor path %q is a symlink; refusing to bind it read-only", path)
		}
		if info.IsDir() {
			return fmt.Errorf(
				"harness config floor path %q is a directory but the catalog expects a file", path)
		}
	}
	return nil
}
