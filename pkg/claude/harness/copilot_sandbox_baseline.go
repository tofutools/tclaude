package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Copilot's minimum sandbox filesystem baseline (TCL-975).
//
// This file answers ONE question and deliberately stops there: which concrete
// paths must a confined GitHub Copilot CLI launch be able to reach, in which
// access mode, and how is each path resolved. It advertises no sandbox
// capability — the descriptor's Sandbox contract stays nil — because the two
// components that will consume this catalog are separate work: Copilot's own
// built-in MXC sandbox (`settings.json` → `sandbox.userPolicy.filesystem`,
// TCL-977) and tclaude's outer bubblewrap/Seatbelt boundary (TCL-978). Both
// need the same answer, so the answer lives once, here, in a form neither one
// owns.
//
// EVERY entry below was observed from the pinned 1.0.77 binary running
// credential-free against the fixture lab's mock provider, and the
// classification of each grant (mandatory vs. tolerated) comes from a
// deny-and-relaunch experiment rather than from reading the CLI's flags:
//
//   - COPILOT_HOME made read-only after a successful first run: the next
//     launch EXITS 1. It is the one hard requirement.
//   - The package cache made read-only after extraction: the launch still
//     succeeds, and the only denied write is a per-launch `inuse.<pid>.lock`
//     the CLI shrugs off. A COLD cache is different — the CLI unpacks its
//     whole platform payload there — so a first launch, and every launch
//     after a version bump, needs write access.
//   - The XDG cache made read-only: the launch still succeeds; only the
//     Microsoft DeveloperTools `deviceid` write is denied.
//
// Path RESOLUTION is not guessed either. It mirrors the CLI's own resolver,
// read out of the pinned launcher and its bundled runtime:
//
//	copilotHome  = COPILOT_HOME ?? $HOME/.copilot
//	packageCache = COPILOT_CACHE_HOME
//	               ?? (darwin: $HOME/Library/Caches/copilot)
//	               ?? ($XDG_CACHE_HOME ?? $HOME/.cache)/copilot
//	deviceIDDir  = ($XDG_CACHE_HOME ?? $HOME/.cache)/Microsoft/DeveloperTools
//
// Note the macOS split, which is the platform difference that matters most
// here and the one an operator is least likely to expect: the package cache
// moves to ~/Library/Caches/copilot, while the device-id cache stays
// XDG-shaped at ~/.cache — the bundled Rust runtime that writes it has no
// darwin branch at all.
//
// What is deliberately NOT in the catalog:
//
//   - The workspace. A Copilot launch obviously needs its working directory,
//     but that grant is the CALLER's (a launch cwd, a repo, a worktree), it is
//     already modelled elsewhere in tclaude, and folding it in here would make
//     a per-harness baseline look like an authority on repository access.
//     CopilotBaselineInput.Workspace exists only so the catalog can REFUSE to
//     hand back a grant that would cover it.
//   - Generic OS runtime prerequisites: the dynamic loader, libc, /proc/self,
//     the CA bundle, PATH directories, the tmpfs the interpreter needs. Those
//     belong to whatever base layer a sandbox implementation already builds
//     for every harness; restating them per-harness would create two sources
//     of truth for the same rows.
//   - Any enterprise or work-organization policy. This is a technical
//     integration baseline.

// CopilotAccess is the access mode one baseline entry needs. It is a set
// rather than an enum because the package cache genuinely needs all three:
// the CLI unpacks its payload there (write), imports it (read), and executes
// binaries out of it (ripgrep, the prebuilt `runtime.node`) — so a sandbox
// that mounts that path noexec breaks tool search even though every byte is
// readable.
type CopilotAccess struct {
	Read    bool
	Write   bool
	Execute bool
}

// String renders the mode in the conventional short form ("r", "rw", "rwx",
// "rx"), which is what the catalog's rendered/logged form shows.
func (a CopilotAccess) String() string {
	var b strings.Builder
	if a.Read {
		b.WriteString("r")
	}
	if a.Write {
		b.WriteString("w")
	}
	if a.Execute {
		b.WriteString("x")
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}

// Access-mode constructors, so a catalog row reads as a mode rather than as
// three booleans.
func copilotReadWrite() CopilotAccess { return CopilotAccess{Read: true, Write: true} }
func copilotReadExec() CopilotAccess  { return CopilotAccess{Read: true, Execute: true} }
func copilotReadWriteExec() CopilotAccess {
	return CopilotAccess{Read: true, Write: true, Execute: true}
}

// CopilotGrantNecessity says how badly a grant is needed, which is the
// difference between a sandbox implementation that must fail a launch and one
// that may narrow it.
type CopilotGrantNecessity string

const (
	// CopilotGrantMandatory: without it a normal launch or resume fails.
	CopilotGrantMandatory CopilotGrantNecessity = "mandatory"

	// CopilotGrantBestEffort: the CLI attempts the access on every launch and
	// tolerates denial. Granting it avoids a denied write per launch; refusing
	// it is safe.
	CopilotGrantBestEffort CopilotGrantNecessity = "best-effort"

	// CopilotGrantFeatureConditional: needed only when the named Feature is in
	// use. A launch without that feature must not carry the grant.
	CopilotGrantFeatureConditional CopilotGrantNecessity = "feature-conditional"
)

// CopilotNodeKind is what the path IS. Both consumers need it and neither can
// infer it reliably: a cold cache directory does not exist yet, so a stat is
// not available, and bwrap binds a directory differently from a file or a
// socket — as does Copilot's own `sandbox.userPolicy.filesystem`.
type CopilotNodeKind string

const (
	CopilotNodeDirectory CopilotNodeKind = "directory"
	CopilotNodeFile      CopilotNodeKind = "file"
	CopilotNodeSocket    CopilotNodeKind = "socket"
)

// Copilot baseline entry ids. Stable vocabulary: a consumer selects rows by id
// (TCL-977 maps them onto Copilot's own policy keys, TCL-978 onto mount rules),
// so these strings are contract, not labels.
const (
	CopilotBaselineStateDir       = "copilot-state-dir"
	CopilotBaselinePackageCache   = "copilot-package-cache"
	CopilotBaselineDeviceIDCache  = "copilot-device-id-cache"
	CopilotBaselineExecutable     = "copilot-executable"
	CopilotBaselineTempDir        = "system-temp-dir"
	CopilotBaselineAgentdSocket   = "tclaude-agentd-socket"
	CopilotBaselineTclaudeBinary  = "tclaude-executable"
	copilotBaselineAgentdFeature  = "tclaude agent coordination (hook callbacks and in-agent `tclaude agent` calls)"
	copilotBaselineTempDirFeature = "shell tools that use the system temp directory (Copilot's own `--disallow-temp-dir` opts out)"
)

// CopilotBaselineEntry is one pre-approved path in the catalog.
type CopilotBaselineEntry struct {
	// ID is the stable selector (one of the CopilotBaseline* constants).
	//
	// It names a KIND of row, not a unique key: a launch with several agentd
	// endpoints produces several rows all carrying
	// CopilotBaselineAgentdSocket. Consumers must treat the catalog as a list
	// and filter by ID — keying a map by ID would silently drop every endpoint
	// but the last.
	ID string

	// Path is the resolved, absolute, cleaned path.
	Path string

	// Source records HOW Path resolved — which environment variable or
	// documented default produced it. A consumer that renders the catalog to
	// an operator shows this, because "why is my home directory in here" is
	// answered by the source, not by the path.
	Source string

	// Kind is the node type at Path.
	Kind CopilotNodeKind

	// Access is the required mode.
	Access CopilotAccess

	// Necessity says whether a launch can proceed without the grant.
	Necessity CopilotGrantNecessity

	// Feature names the feature a CopilotGrantFeatureConditional row belongs
	// to, and is empty otherwise.
	Feature string

	// Purpose is what Copilot does with the path.
	Purpose string

	// Evidence is what proved the row, so a later reader can re-run the
	// experiment rather than trust the comment.
	Evidence string
}

// CopilotBaselineInput is the resolution context for one launch.
//
// Every optional path below (temp dir, sockets, executables) is omitted when
// blank, but a NON-BLANK value that is not absolute fails the whole catalog
// rather than that one row. That is deliberate: a caller passing a junk
// endpoint has a bug in how it resolved the launch, and returning a partial
// catalog would hand a sandbox a grant set that silently lacks a row its
// caller believed was in there.
type CopilotBaselineInput struct {
	// GOOS defaults to runtime.GOOS. Only linux and darwin are supported;
	// anything else is refused rather than approximated.
	GOOS string

	// Home is the launching user's home directory, already resolved and
	// absolute. Required: every default in Copilot's resolver hangs off it,
	// and guessing it is exactly the failure this catalog must not have.
	Home string

	// Getenv reads the launch environment. Defaults to os.Getenv. It is a
	// function rather than a map so a caller composing a launch environment
	// (which may differ from tclaude's own) can pass that environment in.
	Getenv func(string) string

	// TempDir is the system temp directory for the launch, or "" to omit the
	// feature-conditional temp row.
	TempDir string

	// AgentdSockets are the agentd endpoints a coordinating agent may need
	// (canonical first, then retained legacy paths). Empty omits those rows.
	AgentdSockets []string

	// CopilotExecutable is the resolved `copilot` binary, or "" to omit the
	// row. Callers resolve it with exec.LookPath; the catalog does not probe
	// PATH itself, because the PATH that matters is the launch's, not
	// tclaude's.
	CopilotExecutable string

	// TclaudeExecutable is the resolved tclaude binary the hook callbacks and
	// in-agent `tclaude agent` calls invoke, or "" to omit the row.
	TclaudeExecutable string

	// Workspace is the launch working directory (or repository root) when
	// known. It is never granted by this catalog; it exists so the catalog can
	// refuse to return a row that would cover it.
	Workspace string
}

// CopilotSandboxBaseline resolves the pre-approved directory catalog for one
// launch, or refuses.
//
// It fails closed, and the refusals are the point of the function as much as
// the catalog is: an unresolved path, a grant that would cover HOME (or an
// ancestor of it), a grant landing exactly on a shared cache base such as
// ~/.cache, and a grant that would cover the workspace are all errors rather
// than rows. Every one of those is reachable from an operator's own
// environment — COPILOT_HOME=$HOME and COPILOT_CACHE_HOME=~/.cache are both
// things a person can type — and each would silently convert "Copilot runs
// confined" into "Copilot runs with the home directory open".
//
// The returned error is a *SandboxCapabilityError so a daemon can map it to a
// stable wire code.
func CopilotSandboxBaseline(in CopilotBaselineInput) ([]CopilotBaselineEntry, error) {
	goos := in.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "linux" && goos != "darwin" {
		return nil, copilotBaselineError("unsupported-platform",
			fmt.Sprintf("Copilot sandbox baseline is defined for linux and darwin only, not %q", goos))
	}
	getenv := in.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	home := filepath.Clean(strings.TrimSpace(in.Home))
	if home == "" || home == "." || !filepath.IsAbs(home) {
		return nil, copilotBaselineError("unresolved-home",
			fmt.Sprintf("Copilot sandbox baseline needs an absolute home directory, got %q", in.Home))
	}

	stateDir, stateSource := copilotStateDir(getenv, home)
	packageCache, packageSource := copilotPackageCacheDir(goos, getenv, home)
	deviceIDDir, deviceIDSource := copilotDeviceIDCacheDir(getenv, home)

	entries := []CopilotBaselineEntry{
		{
			ID:        CopilotBaselineStateDir,
			Path:      stateDir,
			Source:    stateSource,
			Kind:      CopilotNodeDirectory,
			Access:    copilotReadWrite(),
			Necessity: CopilotGrantMandatory,
			Purpose: "Copilot's configuration and session state: config.json, settings.json, " +
				"session-store.db (plus -wal/-shm), session-state/<session-id>/ " +
				"(session.db, events.jsonl, checkpoints, workspace.yaml), installed-plugins, " +
				"logs/, and tclaude's own hooks/tclaude.json drop-in.",
			Evidence: "Made read-only between two runs of the fixture lab's mock scenario, the " +
				"second launch exits 1. This is the only path whose denial fails a launch outright.",
		},
		{
			ID:     CopilotBaselinePackageCache,
			Path:   packageCache,
			Source: packageSource,
			Kind:   CopilotNodeDirectory,
			// Execute as well as read/write: the payload unpacked here contains
			// the bundled ripgrep binary and prebuilt native modules the CLI
			// loads and runs.
			Access:    copilotReadWriteExec(),
			Necessity: CopilotGrantMandatory,
			Purpose: "The platform payload the CLI unpacks and then runs from: " +
				"pkg/<platform>/<version>/ (app.js, prebuilds/, ripgrep, tgrep, tree-sitter " +
				"grammars), the .extracting-* staging directory it renames into place, and the " +
				"per-launch inuse.<pid>.lock.",
			Evidence: "A COLD cache is written on first launch and after every version bump; a " +
				"warm cache made read-only still launches, denying only inuse.<pid>.lock. " +
				"Granted read/write unconditionally by operator decision, so a supported launch " +
				"never depends on the cache happening to be warm.",
		},
		{
			ID:        CopilotBaselineDeviceIDCache,
			Path:      deviceIDDir,
			Source:    deviceIDSource,
			Kind:      CopilotNodeDirectory,
			Access:    copilotReadWrite(),
			Necessity: CopilotGrantBestEffort,
			Purpose: "The Microsoft DeveloperTools `deviceid` file the bundled runtime writes. " +
				"The grant implies its parent chain being creatable: the runtime makes " +
				"Microsoft/ and DeveloperTools/ itself on a fresh cache.",
			Evidence: "Written on every observed launch; with the directory read-only the launch " +
				"still succeeds. Note this row is XDG-shaped on macOS too — the runtime that " +
				"writes it has no darwin branch, unlike the package cache.",
		},
	}

	if exe := filepath.Clean(strings.TrimSpace(in.CopilotExecutable)); exe != "" && exe != "." {
		entries = append(entries, CopilotBaselineEntry{
			ID:        CopilotBaselineExecutable,
			Path:      exe,
			Source:    "caller-resolved `copilot` on the launch PATH",
			Kind:      CopilotNodeFile,
			Access:    copilotReadExec(),
			Necessity: CopilotGrantMandatory,
			Purpose:   "The launcher the pane executes; it locates or unpacks the package cache above.",
			Evidence:  "Observed as the first exec of every traced launch.",
		})
	}

	if tmp := filepath.Clean(strings.TrimSpace(in.TempDir)); tmp != "" && tmp != "." {
		entries = append(entries, CopilotBaselineEntry{
			ID:        CopilotBaselineTempDir,
			Path:      tmp,
			Source:    "caller-supplied system temp directory",
			Kind:      CopilotNodeDirectory,
			Access:    copilotReadWrite(),
			Necessity: CopilotGrantFeatureConditional,
			Feature:   copilotBaselineTempDirFeature,
			Purpose:   "Scratch space for shell tools the agent runs.",
			Evidence: "The CLI resolves and stats the temp directory at startup and grants it in " +
				"its own default sandbox policy; a launch that never runs a shell tool does not " +
				"need it, which is why Copilot ships `--disallow-temp-dir`.",
		})
	}

	for _, socket := range in.AgentdSockets {
		socket = filepath.Clean(strings.TrimSpace(socket))
		if socket == "" || socket == "." {
			continue
		}
		entries = append(entries, CopilotBaselineEntry{
			ID:        CopilotBaselineAgentdSocket,
			Path:      socket,
			Source:    "caller-supplied agentd endpoint",
			Kind:      CopilotNodeSocket,
			Access:    copilotReadWrite(),
			Necessity: CopilotGrantFeatureConditional,
			Feature:   copilotBaselineAgentdFeature,
			Purpose: "The agentd Unix socket. Connecting to a Unix socket needs read AND write " +
				"on the socket node itself, plus traversal of its parent directory — which is " +
				"why the canonical endpoint lives under ~/.tclaude/api/ and not inside the " +
				"denied private-state subtree ~/.tclaude/data.",
			Evidence: "tclaude's own contract, not a Copilot behavior: hooks/tclaude.json invokes " +
				"a tclaude callback per event, and a coordinating agent runs `tclaude agent …`. " +
				"Both dial this socket.",
		})
	}

	if exe := filepath.Clean(strings.TrimSpace(in.TclaudeExecutable)); exe != "" && exe != "." {
		entries = append(entries, CopilotBaselineEntry{
			ID:        CopilotBaselineTclaudeBinary,
			Path:      exe,
			Source:    "caller-resolved tclaude binary",
			Kind:      CopilotNodeFile,
			Access:    copilotReadExec(),
			Necessity: CopilotGrantFeatureConditional,
			Feature:   copilotBaselineAgentdFeature,
			Purpose:   "The binary a hook callback and an in-agent `tclaude agent` call execute.",
			Evidence:  "tclaude's own contract; the hook command installed by copilotHookInstaller.",
		})
	}

	if err := validateCopilotBaseline(entries, goos, getenv, home, in.Workspace); err != nil {
		return nil, err
	}
	return entries, nil
}

// copilotStateDir mirrors the CLI's `COPILOT_HOME ?? $HOME/.copilot`.
//
// One deliberate deviation: a set-but-BLANK (or whitespace-only) variable
// falls through to the default here, where the CLI's `??` would take the empty
// value and resolve it against the process cwd. The deviation errs in the safe
// direction — the catalog names a directory the CLI might not use (a missing
// grant, caught at launch) rather than granting a cwd-relative path — and the
// alternative is pre-approving a path derived from a working directory.
func copilotStateDir(getenv func(string) string, home string) (path, source string) {
	if dir := strings.TrimSpace(getenv(CopilotHomeEnvVar)); dir != "" {
		return filepath.Clean(dir), CopilotHomeEnvVar
	}
	return filepath.Join(home, ".copilot"), "$HOME/.copilot (documented default)"
}

// CopilotCacheHomeEnvVar is Copilot CLI's override for its package cache. It
// is real but UNDOCUMENTED — it appears in the launcher's resolver and not in
// `copilot help environment` — so tclaude honors it without relying on it.
const CopilotCacheHomeEnvVar = "COPILOT_CACHE_HOME"

// xdgCacheHomeEnvVar is read exactly where Copilot reads it, which on darwin
// is NOT the package cache. See copilotDeviceIDCacheDir.
const xdgCacheHomeEnvVar = "XDG_CACHE_HOME"

// copilotPackageCacheDir mirrors the launcher's own resolver:
//
//	COPILOT_CACHE_HOME
//	 ?? darwin: $HOME/Library/Caches/copilot
//	 ?? ($XDG_CACHE_HOME ?? $HOME/.cache)/copilot
//
// The returned directory is the cache ROOT; the CLI puts its payload in
// <root>/pkg. The root is what gets granted, because the CLI also renames a
// sibling .extracting-* staging directory into place beneath it.
func copilotPackageCacheDir(goos string, getenv func(string) string, home string) (path, source string) {
	if dir := strings.TrimSpace(getenv(CopilotCacheHomeEnvVar)); dir != "" {
		return filepath.Clean(dir), CopilotCacheHomeEnvVar
	}
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Caches", "copilot"),
			"$HOME/Library/Caches/copilot (macOS default)"
	}
	base, baseSource := xdgCacheBase(getenv, home)
	return filepath.Join(base, "copilot"), baseSource + "/copilot"
}

// copilotDeviceIDCacheDir resolves the Microsoft DeveloperTools cache. The
// bundled runtime reads XDG_CACHE_HOME (falling back to $HOME/.cache) on every
// platform — there is no darwin branch — so on macOS this row and the package
// cache above deliberately live in two different trees.
func copilotDeviceIDCacheDir(getenv func(string) string, home string) (path, source string) {
	base, baseSource := xdgCacheBase(getenv, home)
	return filepath.Join(base, "Microsoft", "DeveloperTools"),
		baseSource + "/Microsoft/DeveloperTools"
}

func xdgCacheBase(getenv func(string) string, home string) (path, source string) {
	if dir := strings.TrimSpace(getenv(xdgCacheHomeEnvVar)); dir != "" {
		return filepath.Clean(dir), "$" + xdgCacheHomeEnvVar
	}
	return filepath.Join(home, ".cache"), "$HOME/.cache"
}

// validateCopilotBaseline is the fail-closed gate. It runs over the assembled
// catalog rather than at each construction site so a row added later cannot
// skip it.
func validateCopilotBaseline(
	entries []CopilotBaselineEntry,
	goos string,
	getenv func(string) string,
	home, workspace string,
) error {
	broad := copilotBroadPaths(goos, getenv, home)
	resolvedHome := resolveSymlinks(home)
	resolvedWorkspace := ""
	if ws := strings.TrimSpace(workspace); ws != "" {
		resolvedWorkspace = resolveSymlinks(ws)
	}

	for _, e := range entries {
		if e.Path == "" || !filepath.IsAbs(e.Path) {
			return copilotBaselineError("unresolved-path",
				fmt.Sprintf("Copilot sandbox baseline entry %q did not resolve to an absolute path (got %q)",
					e.ID, e.Path))
		}
		if e.Path != filepath.Clean(e.Path) {
			return copilotBaselineError("unresolved-path",
				fmt.Sprintf("Copilot sandbox baseline entry %q is not a cleaned path: %q", e.ID, e.Path))
		}
		resolved := resolveSymlinks(e.Path)
		if pathContainsOrEqual(resolved, resolvedHome) {
			return copilotBaselineError("too-broad",
				fmt.Sprintf("Copilot sandbox baseline entry %q resolves to %q, which covers the home directory %q; "+
					"a per-harness baseline must never grant HOME", e.ID, e.Path, home))
		}
		for base, label := range broad {
			// Ancestor-or-equal, not equality: with XDG_CACHE_HOME set to
			// ~/.cache/me, a COPILOT_CACHE_HOME of ~/.cache is no longer equal
			// to the base but still swallows it, and every other application's
			// cache with it.
			if pathContainsOrEqual(resolved, base) {
				return copilotBaselineError("too-broad",
					fmt.Sprintf("Copilot sandbox baseline entry %q resolves to %q, which covers the shared %s; "+
						"grant the Copilot-specific subdirectory instead", e.ID, e.Path, label))
			}
		}
		// tclaude's own private state is protected from every harness the same
		// way, so the list is shared with the Codex guard rather than
		// restated. This matters most for the agentd rows: they are
		// caller-supplied, they live under ~/.tclaude, and the whole reason
		// the canonical socket sits in api/ rather than data/ is that data/
		// can then be denied as one subtree. A grant covering data/ would hand
		// a sandboxed agent the daemon's own database.
		for _, sub := range copilotProtectedSubdirs(home) {
			// BOTH directions, unlike the shared-base rule above. An entry
			// that covers the protected tree grants it wholesale; an entry
			// INSIDE it grants a piece of it, which for a daemon database is
			// no better. A socket path under ~/.tclaude/data is the concrete
			// case: it is caller-supplied and would otherwise reach through
			// the very deny the api/ split exists to make possible.
			if pathContainsOrEqual(resolved, sub) || pathContainsOrEqual(sub, resolved) {
				return copilotBaselineError("too-broad",
					fmt.Sprintf("Copilot sandbox baseline entry %q resolves to %q, which covers or lies inside "+
						"tclaude's protected state %q; that tree is denied to every sandboxed harness",
						e.ID, e.Path, sub))
			}
		}
		// A top-level system directory (/etc, /usr, /var, /opt, …) is never a
		// Copilot-specific path, and a read/write row here becomes an rw bind
		// in TCL-978's mount plan. Both spellings are tested — the literal
		// path an operator supplied AND its symlink-resolved form — because
		// either alone has a hole; see copilotSystemRootDir.
		//
		// The temp row's exemption is BY PATH, not by row id. /tmp is
		// top-level and Copilot grants it in its own default policy, so the
		// row has to be able to name it — but "this row may be top-level"
		// would make TMPDIR=/etc a supported way to obtain an rw grant on
		// /etc, which is exactly the hole this rule exists to close.
		systemRoot := copilotSystemRootDir(goos, e.Path) || copilotSystemRootDir(goos, resolved)
		exemptTempRoot := e.ID == CopilotBaselineTempDir &&
			copilotConventionalTempRoot(goos, e.Path, resolved)
		if systemRoot && !exemptTempRoot {
			return copilotBaselineError("too-broad",
				fmt.Sprintf("Copilot sandbox baseline entry %q resolves to %q, a top-level system directory; "+
					"a per-harness baseline grants Copilot-specific subdirectories only", e.ID, e.Path))
		}
		if resolvedWorkspace != "" && pathContainsOrEqual(resolved, resolvedWorkspace) {
			return copilotBaselineError("too-broad",
				fmt.Sprintf("Copilot sandbox baseline entry %q resolves to %q, which covers the workspace %q; "+
					"the workspace grant is the caller's and is never part of this baseline",
					e.ID, e.Path, workspace))
		}
	}
	return nil
}

// copilotBroadPaths are shared bases that must never be granted whole, keyed
// by resolved path and valued by the label used in the refusal message. A
// Copilot path landing exactly on one of these means an environment variable
// pointed at the base instead of at a Copilot-specific subdirectory.
func copilotBroadPaths(goos string, getenv func(string) string, home string) map[string]string {
	xdgBase, _ := xdgCacheBase(getenv, home)
	broad := map[string]string{
		resolveSymlinks(xdgBase):                                "XDG cache base",
		resolveSymlinks(filepath.Join(home, ".local")):          "user data base",
		resolveSymlinks(filepath.Join(home, ".local", "share")): "user data base",
		resolveSymlinks(filepath.Join(home, ".config")):         "user config base",
	}
	if goos == "darwin" {
		broad[resolveSymlinks(filepath.Join(home, "Library"))] = "macOS Library base"
		broad[resolveSymlinks(filepath.Join(home, "Library", "Caches"))] = "macOS caches base"
		// The darwin peer of ~/.local/share. The ~/Library entry above does
		// not cover it: the guard refuses an entry that is an ancestor-or-equal
		// of a base, so a base's own CHILD still has to be listed to be
		// refused.
		broad[resolveSymlinks(filepath.Join(home, "Library", "Application Support"))] =
			"macOS application support base"
	}
	return broad
}

// copilotSystemRootDir reports whether p is the filesystem root or one of its
// immediate children ("/etc", "/usr", "/var", "/home"). Depth is the test
// rather than a name list: a name list would have to guess at every distro's
// and macOS's top-level layout, while "one level under /" is exactly the set
// no Copilot-specific path can legitimately be.
//
// The darwin branch is not a nicety. macOS firmlinks /etc, /tmp and /var onto
// /private/…, so the RESOLVED form of `/etc` is `/private/etc` — two levels
// down, which a naive depth test waves through. That is how a refusal that
// holds on Linux silently stops holding on macOS, which is the worst shape
// this guard could fail in. Callers therefore test both the literal and the
// resolved spelling, and this function normalizes the firmlink prefix so
// either spelling reaches the same verdict.
func copilotSystemRootDir(goos, p string) bool {
	p = copilotNormalizeFirmlink(goos, p)
	if p == string(filepath.Separator) {
		return true
	}
	return filepath.Dir(p) == string(filepath.Separator)
}

// copilotProtectedSubdirs are the absolute trees no Copilot grant may cover.
// It reuses codexProtectedSubdirs (~/.tclaude/data, ~/.codex,
// ~/.claude/sessions) because the protection is tclaude's, not a harness's: a
// list that drifted per harness would mean an operator's private daemon state
// is safe from one agent and not from the next.
func copilotProtectedSubdirs(home string) []string {
	out := make([]string, 0, len(codexProtectedSubdirs))
	for _, sub := range codexProtectedSubdirs {
		out = append(out, resolveSymlinks(filepath.Join(home, sub)))
	}
	return out
}

// copilotConventionalTempRoot reports whether a temp-directory row names the
// one top-level temp directory that is legitimate: /tmp, in either its literal
// or its macOS firmlinked (/private/tmp) spelling.
//
// Every other conventional temp location is already deeper than one level —
// Linux /var/tmp and /run/user/<uid>, macOS /var/folders/<…> — so they never
// reach the system-root rule and need no exemption here.
func copilotConventionalTempRoot(goos, literal, resolved string) bool {
	const conventional = "/tmp"
	return copilotNormalizeFirmlink(goos, literal) == conventional ||
		copilotNormalizeFirmlink(goos, resolved) == conventional
}

// copilotNormalizeFirmlink collapses macOS's /private/<x> firmlink spelling
// onto /<x> so the literal and resolved forms of /etc, /tmp and /var compare
// equal. It is a no-op everywhere else.
func copilotNormalizeFirmlink(goos, p string) string {
	p = filepath.Clean(p)
	if goos != "darwin" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, "/private/"); ok && rest != "" {
		return "/" + rest
	}
	return p
}

func copilotBaselineError(kind, message string) *SandboxCapabilityError {
	return &SandboxCapabilityError{
		Harness: CopilotName,
		Kind:    "copilot-sandbox-baseline-" + kind,
		Message: message,
	}
}
