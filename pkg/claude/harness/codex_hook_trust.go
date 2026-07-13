package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// codexHookEventLabels mirrors codex_hooks::hook_event_key_label. Codex uses
// these snake_case labels both in hook-state keys and in the normalized value
// it hashes. Keep this explicit: a generic case converter is needlessly easy
// to drift for names such as PreToolUse and SubagentStart.
var codexHookEventLabels = map[string]string{
	"PreToolUse":        "pre_tool_use",
	"PermissionRequest": "permission_request",
	"PostToolUse":       "post_tool_use",
	"PreCompact":        "pre_compact",
	"PostCompact":       "post_compact",
	"SessionStart":      "session_start",
	"UserPromptSubmit":  "user_prompt_submit",
	"SubagentStart":     "subagent_start",
	"SubagentStop":      "subagent_stop",
	"Stop":              "stop",
}

var (
	codexVersionPattern = regexp.MustCompile(`\bcodex-cli\s+(\d+)\.(\d+)\.(\d+)\b`)
	codexVersionOutput  = func() ([]byte, error) { return exec.Command("codex", "--version").Output() }
)

// AutoTrustSupported deliberately fails closed outside the Codex versions
// whose private trust normalization tclaude has verified. Unsupported versions
// keep the declarations installed but leave approval to Codex's own /hooks UI.
func (codexHookInstaller) AutoTrustSupported() (bool, string) {
	out, err := codexVersionOutput()
	if err != nil {
		return false, fmt.Sprintf("could not verify Codex hook-trust compatibility: %v", err)
	}
	m := codexVersionPattern.FindSubmatch(out)
	if len(m) != 4 {
		return false, fmt.Sprintf("unrecognized Codex version output %q", strings.TrimSpace(string(out)))
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	patch, _ := strconv.Atoi(string(m[3]))
	if major != 0 || minor < 139 || minor > 144 || (minor == 144 && patch > 1) {
		return false, fmt.Sprintf("Codex %s is outside tclaude's verified hook-trust range (0.139.0–0.144.1)", strings.Join([]string{string(m[1]), string(m[2]), string(m[3])}, "."))
	}
	return true, ""
}

// codexCommandHookHash reproduces Codex's command_hook_hash for the exact
// matcher-less command hook tclaude installs. Codex 0.144.1 builds a normalized
// TOML identity, converts it to canonical JSON, then prefixes its SHA-256 with
// "sha256:". The absent optional TOML fields disappear during conversion;
// timeout and async are normalized to their effective defaults.
//
// Codex does not currently expose a supported CLI that installers can use to
// approve a hook. If its private normalization changes, these records safely
// become stale and Codex asks for review rather than executing changed code.
func codexCommandHookHash(event, command string) (string, error) {
	label, ok := codexHookEventLabels[event]
	if !ok {
		return "", fmt.Errorf("unknown Codex hook event %q", event)
	}
	identity := map[string]any{
		"event_name": label,
		"hooks": []any{map[string]any{
			"async":   false,
			"command": command,
			"timeout": 600,
			"type":    "command",
		}},
	}
	// encoding/json sorts map keys, matching Codex's recursive canonical_json.
	canonical, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode Codex hook identity: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// codexTclaudeHookTrustEntries locates the installed tclaude handlers in the
// final hooks.json shape. The positional group/handler suffix is part of
// Codex's current persisted key, so derive it after the installer has preserved
// and reassembled all co-resident user hooks.
func codexTclaudeHookTrustEntries(
	hooksPath string,
	hooks map[string]json.RawMessage,
	want string,
) ([]codexHookTrustEntry, error) {
	entries := make([]codexHookTrustEntry, 0, len(codexHookEvents))
	for _, event := range codexHookEvents {
		groupsRaw, ok := hooks[event]
		if !ok {
			return nil, fmt.Errorf("codex hook event %s is missing after install", event)
		}
		var groups []codexMatcherGroup
		if err := json.Unmarshal(groupsRaw, &groups); err != nil {
			return nil, fmt.Errorf("parse Codex hook event %s for trust: %w", event, err)
		}
		found := false
		for groupIndex, group := range groups {
			for handlerIndex, hook := range group.Hooks {
				if hook.Command != want {
					continue
				}
				if found {
					return nil, fmt.Errorf("multiple current tclaude hooks found for Codex event %s", event)
				}
				hash, err := codexCommandHookHash(event, want)
				if err != nil {
					return nil, err
				}
				entries = append(entries, codexHookTrustEntry{
					Key:  fmt.Sprintf("%s:%s:%d:%d", hooksPath, codexHookEventLabels[event], groupIndex, handlerIndex),
					Hash: hash,
				})
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("current tclaude hook not found for Codex event %s", event)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

func (codexHookInstaller) InstallTrusted() error {
	if ok, reason := (codexHookInstaller{}).AutoTrustSupported(); !ok {
		return fmt.Errorf("automatic Codex hook trust is unavailable: %s", reason)
	}
	hookPlan, err := planCodexHookInstall()
	if err != nil {
		return err
	}
	if err := validateTrustedCodexHookCommand(hookPlan.want); err != nil {
		return err
	}
	entries, err := codexTclaudeHookTrustEntries(hookPlan.path, hookPlan.hooks, hookPlan.want)
	if err != nil {
		return err
	}
	configPath, err := codexConfigTomlPath()
	if err != nil {
		return err
	}
	// Trust first. If the later atomic hooks.json write fails, the hash has no
	// matching declaration and is inert; writing in the opposite order can
	// leave Codex blocked on startup review.
	if err := ensureCodexHookTrustInFile(configPath, entries); err != nil {
		return fmt.Errorf("write Codex hook trust: %w", err)
	}
	if err := atomicWritePreservingMode(hookPlan.path, hookPlan.out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", hookPlan.path, err)
	}
	return nil
}

func (codexHookInstaller) TrustInstalled() error {
	if ok, reason := (codexHookInstaller{}).AutoTrustSupported(); !ok {
		return fmt.Errorf("automatic Codex hook trust is unavailable: %s", reason)
	}
	path := codexHooksPath()
	hooks, _, err := readCodexHooks(path)
	if err != nil {
		return err
	}
	want := codexHookCommandStr()
	if err := validateTrustedCodexHookCommand(want); err != nil {
		return err
	}
	entries, err := codexTclaudeHookTrustEntries(path, hooks, want)
	if err != nil {
		return err
	}
	configPath, err := codexConfigTomlPath()
	if err != nil {
		return err
	}
	return ensureCodexHookTrustInFile(configPath, entries)
}

func (codexHookInstaller) Trusted() bool {
	if ok, _ := (codexHookInstaller{}).AutoTrustSupported(); !ok {
		return false
	}
	path := codexHooksPath()
	hooks, _, err := readCodexHooks(path)
	if err != nil {
		return false
	}
	want := codexHookCommandStr()
	if validateTrustedCodexHookCommand(want) != nil {
		return false
	}
	entries, err := codexTclaudeHookTrustEntries(path, hooks, want)
	if err != nil {
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

func validateTrustedCodexHookCommand(command string) error {
	executable := firstShellCommandWord(command)
	if !filepath.IsAbs(executable) {
		return fmt.Errorf("refusing automatic Codex hook trust for non-absolute executable %q", executable)
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
