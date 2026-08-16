package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// codexHookTrustEntry is one tclaude hook's persisted Codex trust record.
// Key is Codex's source/event/group/handler identity; Hash fingerprints the
// normalized hook definition that Codex will execute.
type codexHookTrustEntry struct {
	Key  string
	Hash string
}

// AutoTrustSupported only needs an installed Codex binary. The trust operation
// asks that binary's hooks/list endpoint for each hook's authoritative key and
// currentHash; tclaude no longer reproduces version-specific private internals.
func (codexHookInstaller) AutoTrustSupported() (bool, string) {
	if _, err := codexLookPath("codex"); err != nil {
		return false, fmt.Sprintf("could not locate Codex for authoritative hook trust: %v", err)
	}
	return true, ""
}

// codexTclaudeHookTrustEntries asks Codex to identify the tclaude handlers in
// the final on-disk hooks.json. Codex owns both the persisted key and normalized
// hash contract; tclaude filters the response to its exact source and command.
func codexTclaudeHookTrustEntries(hooksPath, want string) ([]codexHookTrustEntry, error) {
	return discoverCodexHookTrustEntries(hooksPath, want)
}

func sortCodexHookTrustEntries(entries []codexHookTrustEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
}

func (codexHookInstaller) InstallTrusted() error {
	return withCodexHooksInstallLock(func() (retErr error) {
		hookPlan, err := planCodexHookInstall()
		if err != nil {
			return err
		}
		if err := validateTrustedCodexHookCommand(hookPlan.want); err != nil {
			return err
		}
		if err := validateExactCodexTclaudeHooks(hookPlan.hooks, hookPlan.want); err != nil {
			return fmt.Errorf("validate planned Codex hooks for trust: %w", err)
		}
		if ok, reason := (codexHookInstaller{}).AutoTrustSupported(); !ok {
			return fmt.Errorf("automatic Codex hook trust is unavailable: %s", reason)
		}
		configPath, err := codexConfigTomlPath()
		if err != nil {
			return err
		}
		if err := preflightCodexHookTrustFile(configPath, hookPlan.path); err != nil {
			return fmt.Errorf("preflight Codex hook trust: %w", err)
		}
		backup, err := snapshotCodexHooksFile(hookPlan.path)
		if err != nil {
			return fmt.Errorf("snapshot Codex hooks before trusted install: %w", err)
		}
		if err := atomicWritePreservingMode(hookPlan.path, hookPlan.out, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", hookPlan.path, err)
		}
		defer func() {
			if retErr == nil {
				return
			}
			if rollbackErr := backup.restoreIfUnchanged(hookPlan.out); rollbackErr != nil {
				retErr = fmt.Errorf("%w; additionally failed to roll back Codex hooks: %v", retErr, rollbackErr)
			}
		}()
		if err := backup.validateInstalledState(hookPlan.out); err != nil {
			return fmt.Errorf("validate installed Codex hooks state: %w", err)
		}
		// Ask Codex only after the final declaration is on disk. A discovery or
		// trust-write failure leaves the hook untrusted and therefore fail-closed.
		entries, err := codexTclaudeHookTrustEntries(hookPlan.path, hookPlan.want)
		if err != nil {
			return err
		}
		if err := backup.validateInstalledState(hookPlan.out); err != nil {
			return fmt.Errorf("Codex hooks changed during authoritative discovery: %w", err)
		}
		if err := ensureCodexHookTrustInFile(configPath, entries); err != nil {
			return fmt.Errorf("write Codex hook trust: %w", err)
		}
		return nil
	})
}

type codexHooksFileSnapshot struct {
	path    string
	target  string
	data    []byte
	perm    os.FileMode
	newPerm os.FileMode
	existed bool
}

func snapshotCodexHooksFile(path string) (codexHooksFileSnapshot, error) {
	target, err := atomicWriteTarget(path)
	if err != nil {
		return codexHooksFileSnapshot{}, err
	}
	snapshot := codexHooksFileSnapshot{path: path, target: target, perm: 0o644, newPerm: 0o644}
	data, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return snapshot, nil
		}
		return codexHooksFileSnapshot{}, err
	}
	snapshot.data = data
	snapshot.existed = true
	if info, err := os.Stat(target); err == nil {
		snapshot.perm = info.Mode().Perm()
		snapshot.newPerm = snapshot.perm
	}
	return snapshot, nil
}

func (s codexHooksFileSnapshot) validateInstalledState(installed []byte) error {
	target, err := atomicWriteTarget(s.path)
	if err != nil {
		return err
	}
	if target != s.target {
		return fmt.Errorf("%s target changed from %s to %s", s.path, s.target, target)
	}
	info, err := os.Stat(s.target)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != s.newPerm {
		return fmt.Errorf("%s installed with unexpected mode %04o (want %04o)",
			s.target, info.Mode().Perm(), s.newPerm)
	}
	current, err := os.ReadFile(s.target)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, installed) {
		return fmt.Errorf("%s contents changed; refusing trust", s.target)
	}
	return nil
}

// restoreIfUnchanged rolls back only while the file still contains the bytes
// this install wrote. A concurrent external edit wins rather than being
// overwritten by error recovery.
func (s codexHooksFileSnapshot) restoreIfUnchanged(installed []byte) error {
	if err := s.validateUnchanged(installed); err != nil {
		return err
	}
	if !s.existed {
		return os.Remove(s.target)
	}
	return atomicWriteFile(s.target, s.data, s.perm)
}

func (s codexHooksFileSnapshot) validateUnchanged(expected []byte) error {
	target, err := atomicWriteTarget(s.path)
	if err != nil {
		return err
	}
	if target != s.target {
		return fmt.Errorf("%s target changed from %s to %s; refusing rollback", s.path, s.target, target)
	}
	info, err := os.Stat(s.target)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != s.newPerm {
		return fmt.Errorf("%s mode changed from %04o to %04o; refusing rollback",
			s.target, s.newPerm, info.Mode().Perm())
	}
	current, err := os.ReadFile(s.target)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return fmt.Errorf("%s changed concurrently; refusing to overwrite it", s.target)
	}
	return nil
}

func (codexHookInstaller) TrustInstalled() error {
	return withCodexHooksInstallLock(func() error {
		path := codexHooksPath()
		snapshot, err := snapshotCodexHooksFile(path)
		if err != nil {
			return err
		}
		if !snapshot.existed {
			return fmt.Errorf("Codex hooks file %s does not exist", path)
		}
		hooks, _, err := decodeCodexHooks(snapshot.data, path)
		if err != nil {
			return err
		}
		if err := snapshot.validateUnchanged(snapshot.data); err != nil {
			return fmt.Errorf("Codex hooks changed during authoritative discovery: %w", err)
		}
		want := codexHookCommandStr()
		if err := validateTrustedCodexHookCommand(want); err != nil {
			return err
		}
		if err := validateExactCodexTclaudeHooks(hooks, want); err != nil {
			return err
		}
		if ok, reason := (codexHookInstaller{}).AutoTrustSupported(); !ok {
			return fmt.Errorf("automatic Codex hook trust is unavailable: %s", reason)
		}
		entries, err := codexTclaudeHookTrustEntries(path, want)
		if err != nil {
			return err
		}
		if err := snapshot.validateUnchanged(snapshot.data); err != nil {
			return fmt.Errorf("Codex hooks changed during authoritative discovery: %w", err)
		}
		configPath, err := codexConfigTomlPath()
		if err != nil {
			return err
		}
		return ensureCodexHookTrustInFile(configPath, entries)
	})
}

func (codexHookInstaller) Trusted() bool {
	if ok, _ := (codexHookInstaller{}).AutoTrustSupported(); !ok {
		return false
	}
	path := codexHooksPath()
	snapshot, err := snapshotCodexHooksFile(path)
	if err != nil || !snapshot.existed {
		return false
	}
	hooks, _, err := decodeCodexHooks(snapshot.data, path)
	if err != nil {
		return false
	}
	want := codexHookCommandStr()
	if validateTrustedCodexHookCommand(want) != nil {
		return false
	}
	if validateExactCodexTclaudeHooks(hooks, want) != nil {
		return false
	}
	entries, err := codexTclaudeHookTrustEntries(path, want)
	if err != nil {
		return false
	}
	if snapshot.validateUnchanged(snapshot.data) != nil {
		return false
	}
	configPath, err := codexConfigTomlPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return false
	}
	changed, _, err := planCodexHookTrust(data, entries)
	return err == nil && !changed
}

func validateExactCodexTclaudeHooks(hooks map[string]json.RawMessage, want string) error {
	desired := make(map[string]bool, len(desiredCodexHookEvents()))
	for _, event := range desiredCodexHookEvents() {
		desired[event] = true
	}
	for event, groups := range hooks {
		if codexHooksNeedCleanup(groups, want) {
			return fmt.Errorf("Codex hook declarations differ from tclaude's exact managed shape; repair them before trust")
		}
		if !desired[event] && codexHooksContain(groups, want) {
			return fmt.Errorf("stale tclaude hook declaration remains for non-required Codex event %s", event)
		}
	}
	for _, event := range desiredCodexHookEvents() {
		if !codexHooksContain(hooks[event], want) {
			return fmt.Errorf("exact tclaude hook declaration is missing for Codex event %s", event)
		}
	}
	return nil
}

func validateTrustedCodexHookCommand(command string) error {
	executable := firstShellCommandWord(command)
	if executable != "tclaude" && !filepath.IsAbs(executable) {
		return fmt.Errorf(
			"refusing automatic Codex hook trust for non-portable relative executable %q",
			executable)
	}
	return nil
}

// ensureCodexHookTrustInFile atomically trusts only the supplied installed
// hooks. A missing config is treated as empty; unrelated configuration and
// explicit enabled=false state are preserved.
func ensureCodexHookTrustInFile(configPath string, entries []codexHookTrustEntry) error {
	return EditCodexConfigFile(configPath, 0o644, func(data []byte) (bool, []byte, error) {
		return planCodexHookTrust(data, entries)
	})
}

func preflightCodexHookTrustFile(configPath, hooksPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	const zeroHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	_, _, err = planCodexHookTrust(data, []codexHookTrustEntry{{
		Key: hooksPath + ":tclaude_preflight:0:0", Hash: zeroHash,
	}})
	return err
}

func atomicWritePreservingMode(path string, data []byte, defaultPerm os.FileMode) error {
	target, err := atomicWriteTarget(path)
	if err != nil {
		return err
	}
	perm := defaultPerm
	if fi, statErr := os.Stat(target); statErr == nil {
		perm = fi.Mode().Perm()
	}
	return atomicWriteFile(target, data, perm)
}

func atomicWriteTarget(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", path, err)
	}
	return target, nil
}

// planCodexHookTrust is the pure, line-preserving trust editor. Codex itself
// writes one table per hook below hooks.state; we update that exact shape and
// refuse unusual conflicting TOML forms rather than risking a duplicate key.
func planCodexHookTrust(data []byte, entries []codexHookTrustEntry) (bool, []byte, error) {
	if _, err := parseCodexTOML(data); err != nil {
		return false, nil, fmt.Errorf("parse Codex config before hook-trust edit: %w", err)
	}
	ordered := append([]codexHookTrustEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
	out := data
	changedAny := false
	for _, entry := range ordered {
		if entry.Key == "" || !strings.HasPrefix(entry.Hash, "sha256:") {
			return false, nil, fmt.Errorf("invalid Codex hook trust entry for %q", entry.Key)
		}
		changed, next, err := planOneCodexHookTrust(out, entry)
		if err != nil {
			return false, nil, err
		}
		if changed {
			changedAny = true
			out = next
		}
	}
	if !changedAny {
		return false, data, nil
	}
	root, err := parseCodexTOML(out)
	if err != nil {
		return false, nil, fmt.Errorf("validate Codex config after hook-trust edit: %w", err)
	}
	for _, entry := range ordered {
		state, exists, err := semanticCodexHookState(root, entry.Key)
		if err != nil || !exists || state["trusted_hash"] != entry.Hash {
			return false, nil, fmt.Errorf("validate Codex hook trust for %q after edit", entry.Key)
		}
	}
	return true, out, nil
}

func planOneCodexHookTrust(data []byte, entry codexHookTrustEntry) (bool, []byte, error) {
	root, err := parseCodexTOML(data)
	if err != nil {
		return false, nil, err
	}
	_, semanticallyExists, err := semanticCodexHookState(root, entry.Key)
	if err != nil {
		return false, nil, err
	}
	lines, sep := splitConfigLines(data)
	structural := tomlStructuralLines(lines)
	wantTable := "hooks.state." + tomlQuote(entry.Key)
	header := "[" + wantTable + "]"
	wantLine := "trusted_hash = " + tomlQuote(entry.Hash)

	hdrIdx := -1
	for i, raw := range lines {
		if semanticallyExists && structural[i] {
			name, ok := tomlTableHeader(raw)
			if !ok || name != wantTable {
				continue
			}
			if hdrIdx != -1 {
				return false, nil, fmt.Errorf("codex hook trust: duplicate table %s", header)
			}
			hdrIdx = i
		}
	}
	if hdrIdx == -1 {
		if semanticallyExists {
			return false, nil, fmt.Errorf("codex hook trust: hook key %q uses a valid but non-standard TOML form tclaude will not rewrite", entry.Key)
		}
		out := append([]string{}, lines...)
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, header, wantLine)
		return true, joinConfigLines(out, sep), nil
	}

	bodyEnd := len(lines)
	for i := hdrIdx + 1; i < len(lines); i++ {
		if !structural[i] {
			continue
		}
		if _, ok := tomlTableHeader(lines[i]); ok {
			bodyEnd = i
			break
		}
		if _, ok := tomlArrayTableHeader(lines[i]); ok {
			bodyEnd = i
			break
		}
	}
	hashIdx := -1
	for i := hdrIdx + 1; i < bodyEnd; i++ {
		if !structural[i] {
			continue
		}
		key, _, ok := tomlKeyValue(lines[i])
		if !ok || key != "trusted_hash" {
			continue
		}
		if hashIdx != -1 {
			return false, nil, fmt.Errorf("codex hook trust: duplicate trusted_hash in %s", header)
		}
		hashIdx = i
	}
	if hashIdx == -1 {
		out := append([]string{}, lines[:hdrIdx+1]...)
		out = append(out, wantLine)
		out = append(out, lines[hdrIdx+1:]...)
		return true, joinConfigLines(out, sep), nil
	}
	if tomlStringValueIs(lines[hashIdx], entry.Hash) {
		return false, data, nil
	}
	indent := lines[hashIdx][:len(lines[hashIdx])-len(strings.TrimLeft(lines[hashIdx], " \t"))]
	out := append([]string{}, lines[:hashIdx]...)
	out = append(out, indent+wantLine)
	out = append(out, lines[hashIdx+1:]...)
	return true, joinConfigLines(out, sep), nil
}

func parseCodexTOML(data []byte) (map[string]any, error) {
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return root, nil
	}
	if err := toml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	return root, nil
}

func semanticCodexHookState(root map[string]any, entryKey string) (map[string]any, bool, error) {
	hooksRaw, ok := root["hooks"]
	if !ok {
		return nil, false, nil
	}
	hooks, ok := hooksRaw.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("codex hook trust: hooks is not a TOML table")
	}
	stateRaw, ok := hooks["state"]
	if !ok {
		return nil, false, nil
	}
	state, ok := stateRaw.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("codex hook trust: hooks.state is not a TOML table")
	}
	entryRaw, ok := state[entryKey]
	if !ok {
		return nil, false, nil
	}
	entry, ok := entryRaw.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("codex hook trust: state for %q is not a TOML table", entryKey)
	}
	return entry, true, nil
}

// tomlStructuralLines marks lines whose first token is outside a multiline
// basic/literal string. The semantic parser above decides whether a trust table
// actually exists; this small lexer only prevents locating that real table or
// its body through header-looking text inside a multiline value.
func tomlStructuralLines(lines []string) []bool {
	out := make([]bool, len(lines))
	var delimiter string
	for i, line := range lines {
		out[i] = delimiter == ""
		for pos := 0; pos+2 < len(line); {
			if delimiter == "" {
				double := strings.Index(line[pos:], `"""`)
				single := strings.Index(line[pos:], `'''`)
				switch {
				case double < 0 && single < 0:
					pos = len(line)
				case single >= 0 && (double < 0 || single < double):
					delimiter, pos = `'''`, pos+single+3
				default:
					delimiter, pos = `"""`, pos+double+3
				}
				continue
			}
			idx := strings.Index(line[pos:], delimiter)
			if idx < 0 {
				break
			}
			pos += idx + 3
			delimiter = ""
		}
	}
	return out
}
