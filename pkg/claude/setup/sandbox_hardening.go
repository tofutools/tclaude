package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// sandboxHardeningSpec returns the entries the agent-sandbox hardening
// guide (docs/sandbox-hardening.md) recommends adding to the user-level
// Claude Code settings file, expressed as a generic JSON tree.
//
// It is a faithful copy of that doc's recommended config block — the
// doc is the source of truth, so this must be kept in lockstep with it.
// The `sandbox` sub-tree comes from harness.ClaudeSandboxOnBlockForGOOS(), the
// SAME block the per-session `--sandbox on` spawn mode injects via
// `--settings`, so the global hardening and the per-session override can
// never drift. Its Unix-socket policy is platform-specific: Linux requires
// `allowAllUnixSockets`, while macOS must leave that broad switch absent so
// its exact `allowUnixSockets` list remains effective. The `permissions`
// deny-list below is hardening-only — a global settings concern, not a
// per-session sandbox knob — so it stays here.
//
// Arrays are []any (not []string) so the merge engine compares and
// appends them uniformly against values decoded from the user's file,
// where every JSON array is a []any.
func sandboxHardeningSpec() map[string]any {
	return sandboxHardeningSpecForGOOS(runtime.GOOS)
}

func sandboxHardeningSpecForGOOS(goos string) map[string]any {
	return map[string]any{
		"sandbox": harness.ClaudeSandboxOnBlockForGOOS(goos),
		"permissions": map[string]any{
			// The tool-permission layer mirrors the OS-sandbox deny above. It
			// matters independently: it still blocks the Read/Edit tools when
			// the OS sandbox is disabled.
			"deny": []any{
				"Edit(~/.tclaude/data/**)",
				"Read(~/.tclaude/data/**)",
				"Edit(~/.claude/sessions/**)",
				"Read(~/.claude/sessions/**)",
			},
		},
	}
}

// hardeningReport accumulates what an install did so the caller can print a
// summary. The merge is append-only except for the targeted macOS legacy
// socket migration, so this records additions, that update, what was already
// in place, and the conflicts it deliberately left alone.
type hardeningReport struct {
	// added is one human-readable line per scalar key or array element
	// the merge appended, in deterministic depth-first order.
	added []string
	// alreadyPresent is one line per spec entry the file already had
	// with the value the hardening wants — a second run fills this and
	// leaves added empty (idempotency).
	alreadyPresent []string
	// updated records the one security migration allowed to replace a
	// previously installed scalar: macOS allowAllUnixSockets true -> false.
	updated []string
	// scalarConflicts is one line per scalar key present with a
	// DIFFERENT value than the hardening wants. Left unchanged — the
	// human is warned to fix it themselves.
	scalarConflicts []string
	// typeConflicts is one line per key whose existing value has an
	// incompatible JSON type (e.g. a string where an array is needed).
	// That node is skipped, never clobbered.
	typeConflicts []string
}

// changed reports whether the install mutated the tree. Additions and the
// targeted macOS update mutate it; conflicts and "already present" entries do
// not. When false the caller must not rewrite the file — that is what keeps a
// second run a true no-op.
func (r *hardeningReport) changed() bool { return len(r.added) > 0 || len(r.updated) > 0 }

// hardeningResult is the outcome of applySandboxHardening: the report
// plus the file-level side effects the caller turns into output.
type hardeningResult struct {
	report       *hardeningReport
	settingsPath string
	backupPath   string // path of the timestamped backup, or "" if none was made
	wrote        bool   // true if the settings file was rewritten
}

// mergeHardening recursively merges the desired spec sub-tree into the
// existing JSON tree, mutating existing in place. path is the dotted
// key path of existing for human-readable report messages ("" at root).
//
// It is strictly append-only and never removes or overwrites:
//   - missing object  -> an empty object is created and recursed into;
//   - missing array   -> the whole desired array is added;
//   - existing array  -> missing elements are appended (deduped);
//   - missing scalar  -> the scalar is added;
//   - existing scalar -> kept; a differing value is a recorded conflict;
//   - type mismatch   -> the node is skipped and recorded, never clobbered.
//
// Keys are visited in sorted order so the report is deterministic.
func mergeHardening(path string, existing, desired map[string]any, r *hardeningReport) {
	for _, key := range sortedKeys(desired) {
		want := desired[key]
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		cur, present := existing[key]

		switch wantTyped := want.(type) {
		case map[string]any:
			if !present {
				child := map[string]any{}
				existing[key] = child
				mergeHardening(childPath, child, wantTyped, r)
				continue
			}
			curObj, ok := cur.(map[string]any)
			if !ok {
				r.typeConflicts = append(r.typeConflicts, fmt.Sprintf(
					"%s: hardening expects a JSON object but settings.json has %s — skipped",
					childPath, jsonKind(cur)))
				continue
			}
			mergeHardening(childPath, curObj, wantTyped, r)

		case []any:
			if !present {
				existing[key] = append([]any{}, wantTyped...)
				for _, elem := range wantTyped {
					r.added = append(r.added, fmt.Sprintf("%s += %s", childPath, formatVal(elem)))
				}
				continue
			}
			curArr, ok := cur.([]any)
			if !ok {
				r.typeConflicts = append(r.typeConflicts, fmt.Sprintf(
					"%s: hardening expects a JSON array but settings.json has %s — skipped",
					childPath, jsonKind(cur)))
				continue
			}
			merged := curArr
			for _, elem := range wantTyped {
				if containsVal(merged, elem) {
					r.alreadyPresent = append(r.alreadyPresent,
						fmt.Sprintf("%s already contains %s", childPath, formatVal(elem)))
					continue
				}
				merged = append(merged, elem)
				r.added = append(r.added, fmt.Sprintf("%s += %s", childPath, formatVal(elem)))
			}
			existing[key] = merged

		default: // scalar (bool / number / string / null)
			if !present {
				existing[key] = want
				r.added = append(r.added, fmt.Sprintf("%s = %s", childPath, formatVal(want)))
				continue
			}
			if isContainer(cur) {
				r.typeConflicts = append(r.typeConflicts, fmt.Sprintf(
					"%s: hardening expects a scalar but settings.json has %s — skipped",
					childPath, jsonKind(cur)))
				continue
			}
			if reflect.DeepEqual(cur, want) {
				r.alreadyPresent = append(r.alreadyPresent,
					fmt.Sprintf("%s already %s", childPath, formatVal(want)))
				continue
			}
			r.scalarConflicts = append(r.scalarConflicts, fmt.Sprintf(
				"%s: hardening wants %s but settings.json already has %s — left unchanged (fix it manually)",
				childPath, formatVal(want), formatVal(cur)))
		}
	}
}

// loadSettingsTree reads settingsPath as a generic JSON object tree so
// every key — including ones tclaude knows nothing about — round-trips
// untouched. A missing or empty file (or a literal `null`) is treated
// as an empty object. A file whose top-level JSON is not an object
// (e.g. an array or string) is an error: rewriting it would lose data.
func loadSettingsTree(settingsPath string) (map[string]any, error) {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read settings: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var tree map[string]any
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("parse %s: %w", settingsPath, err)
	}
	if tree == nil { // file held a literal `null`
		return map[string]any{}, nil
	}
	return tree, nil
}

// settingsFileMode returns path's permission bits, or 0o644 if path does
// not exist or cannot be stat'd.
//
// It is used so both the backup and the rewrite of the settings file
// carry the original file's mode explicitly, rather than relying on
// os.WriteFile's subtle behaviour (it applies the perm argument only
// when *creating* a file, and ignores it for an existing one). Making
// the intent explicit keeps a private 0600 settings.json private and
// future-proofs an eventual atomic temp-file+rename rewrite, where the
// temp file is new and would otherwise be created 0644.
func settingsFileMode(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o644
}

// backupSettings copies settingsPath to a timestamped sibling
// (settings.json.bak-YYYYMMDD-HHMMSS) and returns the backup path. If
// the settings file does not exist yet there is nothing to back up and
// it returns "" with no error.
//
// The backup inherits the original file's permission bits, so a
// private (0600) settings.json is never copied into a world-readable
// backup.
func backupSettings(settingsPath string) (string, error) {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	backupPath := settingsPath + ".bak-" + time.Now().Format("20060102-150405")
	if err := os.WriteFile(backupPath, data, settingsFileMode(settingsPath)); err != nil {
		return "", err
	}
	return backupPath, nil
}

// applySandboxHardening loads the Claude Code settings file at settingsPath,
// applies the platform hardening, and — only if it changed something — backs
// the file up and rewrites it. The ordinary merge is append-only. On macOS it
// also migrates the legacy allowAllUnixSockets=true value to false so the
// exact socket allowlist becomes effective.
//
// Taking settingsPath explicitly keeps this end-to-end-testable against
// a temp file; installSandboxHardening is the thin production wrapper.
func applySandboxHardening(settingsPath string) (*hardeningResult, error) {
	return applySandboxHardeningForGOOS(settingsPath, runtime.GOOS)
}

func applySandboxHardeningForGOOS(settingsPath, goos string) (*hardeningResult, error) {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return nil, fmt.Errorf("create settings directory: %w", err)
	}

	tree, err := loadSettingsTree(settingsPath)
	if err != nil {
		return nil, err
	}

	r := &hardeningReport{}
	if goos == "darwin" {
		disableLegacyMacOSAllowAllUnixSockets(tree, r)
	}
	mergeHardening("", tree, sandboxHardeningSpecForGOOS(goos), r)

	res := &hardeningResult{report: r, settingsPath: settingsPath}
	if !r.changed() {
		// Nothing was added — leave the file exactly as it is so a
		// repeat run is a true no-op (and skip the spurious backup).
		return res, nil
	}

	// Capture the original file's mode before we touch it, so the
	// rewrite preserves it (a private 0600 settings.json stays 0600);
	// a genuinely new file gets the 0o644 default.
	mode := settingsFileMode(settingsPath)

	backupPath, err := backupSettings(settingsPath)
	if err != nil {
		return nil, fmt.Errorf("back up settings: %w", err)
	}
	res.backupPath = backupPath

	output, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("serialize settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, output, mode); err != nil {
		return nil, fmt.Errorf("write settings: %w", err)
	}
	res.wrote = true
	return res, nil
}

// disableLegacyMacOSAllowAllUnixSockets repairs settings written by older
// tclaude releases. On macOS, allowAllUnixSockets=true disables the path
// allowlist that keeps Claude away from host-control sockets. This is the sole
// overwrite performed by the hardening installer: it is narrow, backed up by
// applySandboxHardeningForGOOS, and idempotent.
func disableLegacyMacOSAllowAllUnixSockets(tree map[string]any, r *hardeningReport) {
	sandbox, ok := tree["sandbox"].(map[string]any)
	if !ok {
		return
	}
	network, ok := sandbox["network"].(map[string]any)
	if !ok {
		return
	}
	value, present := network["allowAllUnixSockets"]
	if !present {
		return
	}
	enabled, ok := value.(bool)
	if !ok {
		r.typeConflicts = append(r.typeConflicts,
			"sandbox.network.allowAllUnixSockets: macOS hardening expects false or omission but settings.json has "+
				jsonKind(value)+" — skipped")
		return
	}
	if !enabled {
		return
	}
	network["allowAllUnixSockets"] = false
	r.updated = append(r.updated,
		"sandbox.network.allowAllUnixSockets = false (was true; macOS uses allowUnixSockets as an allowlist)")
}

// installSandboxHardening is the --install-sandbox-hardening entry
// point: it resolves the user-level Claude Code settings path, applies
// the hardening, and prints a summary of what changed.
func installSandboxHardening() error {
	if err := sandboxHardeningSocketMigrationError(); err != nil {
		return err
	}
	settingsPath := session.ClaudeSettingsPath()
	if settingsPath == "" {
		return fmt.Errorf("cannot determine Claude settings path")
	}

	res, err := applySandboxHardening(settingsPath)
	if err != nil {
		return err
	}
	printHardeningReport(res)
	return nil
}

func sandboxHardeningSocketMigrationError() error {
	canonical := agentipc.CanonicalSocketPath()
	if explicit := agentipc.ExplicitSocketPath(); explicit != "" && explicit != canonical {
		return fmt.Errorf("sandbox hardening requires the canonical agentd socket %s; "+
			"custom socket %s is unsupported", canonical, explicit)
	}
	if !agentipc.SocketReachable(canonical) && agentipc.AnyLegacySocketReachable() {
		return fmt.Errorf("agentd is still listening only on a pre-split legacy socket (%v); "+
			"restart agentd after upgrading tclaude before installing sandbox hardening",
			agentipc.LegacySocketPaths())
	}
	return nil
}

// printHardeningReport writes the human-facing summary of a hardening
// merge: every addition, a count of entries already in place, and a
// prominent warning per conflict the merge deliberately did not touch.
func printHardeningReport(res *hardeningResult) {
	r := res.report

	for _, line := range r.added {
		fmt.Printf("✓ Added %s\n", line)
	}
	for _, line := range r.updated {
		fmt.Printf("✓ Updated %s\n", line)
	}
	if n := len(r.alreadyPresent); n > 0 {
		fmt.Printf("✓ %d hardening %s already present\n", n, pluralEntries(n))
	}
	for _, line := range r.scalarConflicts {
		fmt.Printf("⚠ %s\n", line)
	}
	for _, line := range r.typeConflicts {
		fmt.Printf("⚠ %s\n", line)
	}

	if res.backupPath != "" {
		fmt.Printf("  Backed up existing settings to %s\n", res.backupPath)
	}
	if res.wrote {
		fmt.Printf("✓ Sandbox hardening written to %s\n", res.settingsPath)
	} else {
		fmt.Println("✓ Sandbox hardening already in place — no changes")
	}
	if len(r.scalarConflicts) > 0 || len(r.typeConflicts) > 0 {
		fmt.Println("  Review the ⚠ warnings above — those entries were left as-is " +
			"and the hardening is incomplete until you resolve them.")
	}
}

// --- small JSON-tree helpers -------------------------------------------------

// sortedKeys returns m's keys in sorted order, for deterministic walks.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// containsVal reports whether arr already holds a value deeply equal to
// want — the dedupe check for append-only array merges.
func containsVal(arr []any, want any) bool {
	for _, elem := range arr {
		if reflect.DeepEqual(elem, want) {
			return true
		}
	}
	return false
}

// isContainer reports whether v is a JSON object or array (as opposed
// to a scalar / null).
func isContainer(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

// jsonKind names the JSON kind of a decoded value, for conflict messages.
func jsonKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "a JSON object"
	case []any:
		return "a JSON array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("a %T", v)
	}
}

// formatVal renders a decoded JSON value compactly for report messages
// (`true`, `"~/.tclaude"`, …).
func formatVal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// pluralEntries returns "entry" for n == 1 and "entries" otherwise.
func pluralEntries(n int) string {
	if n == 1 {
		return "entry"
	}
	return "entries"
}
