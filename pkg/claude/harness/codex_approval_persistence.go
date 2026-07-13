package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

var codexAppIDRe = regexp.MustCompile(`^asdk_app_[A-Za-z0-9]+$`)

// CodexToolApproval is one app-tool "Always allow" choice that Codex
// appended to a launch-specific config profile.
type CodexToolApproval struct {
	AppID string
	Tool  string
}

// CodexApprovalPromotion reports what a launch-profile reconciliation found.
// Conflicts are deliberately not overwritten: an existing global decision is
// user-owned and wins over the temporary profile.
type CodexApprovalPromotion struct {
	Found     int
	Added     int
	Existing  int
	Conflicts []string
}

type codexLaunchProfileValidationError struct{ err error }

func (e *codexLaunchProfileValidationError) Error() string { return e.err.Error() }
func (e *codexLaunchProfileValidationError) Unwrap() error { return e.err }

// IsCodexLaunchProfileValidationError reports whether promotion failed because
// the managed profile itself was malformed or its sealed baseline changed.
// Such a profile is unsafe to retain even though transient global-config
// failures should preserve it for a later retry.
func IsCodexLaunchProfileValidationError(err error) bool {
	var target *codexLaunchProfileValidationError
	return errors.As(err, &target)
}

// CodexConfigDir exposes the directory where Codex resolves config.toml and
// <name>.config.toml. It follows CODEX_HOME, matching the Codex CLI.
func CodexConfigDir() (string, error) { return codexConfigDir() }

// IsCodexAgentLaunchProfilePath reports whether path is a direct child of the
// active Codex config directory with tclaude's launch-profile filename shape.
func IsCodexAgentLaunchProfilePath(path string) bool {
	dir, err := codexConfigDir()
	if err != nil {
		return false
	}
	clean := filepath.Clean(path)
	return filepath.Clean(filepath.Dir(clean)) == filepath.Clean(dir) &&
		codexAgentLaunchProfileFileRe.MatchString(filepath.Base(clean))
}

// ExtractCodexLaunchProfileApprovals verifies the sealed tclaude baseline and
// returns only exact app-tool approval additions written after that baseline.
// Unrelated Codex-owned additions (for example TUI onboarding state) are
// ignored, while any mutation to the baseline prefix fails closed.
func ExtractCodexLaunchProfileApprovals(data []byte) ([]CodexToolApproval, error) {
	marker := []byte(codexAgentProfileBaselineMarker)
	idx := bytes.Index(data, marker)
	if idx < 0 || (idx > 0 && data[idx-1] != '\n') {
		return nil, fmt.Errorf("managed Codex profile has no baseline seal")
	}
	if bytes.Contains(data[idx+len(marker):], marker) {
		return nil, fmt.Errorf("managed Codex profile has multiple baseline seals")
	}
	lineEndRel := bytes.IndexByte(data[idx:], '\n')
	if lineEndRel < 0 {
		return nil, fmt.Errorf("managed Codex profile has an unterminated baseline seal")
	}
	lineEnd := idx + lineEndRel
	wantHex := strings.TrimSpace(string(data[idx+len(marker) : lineEnd]))
	if len(wantHex) != sha256.Size*2 {
		return nil, fmt.Errorf("managed Codex profile has an invalid baseline seal")
	}
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		return nil, fmt.Errorf("managed Codex profile has an invalid baseline seal: %w", err)
	}
	got := sha256.Sum256(data[:idx])
	if !bytes.Equal(want, got[:]) {
		return nil, fmt.Errorf("managed Codex profile baseline changed; refusing approval promotion")
	}

	// Parse the whole document as well as the appended suffix. Whole-document
	// parsing rejects a suffix that redefines a baseline key/table. Parsing the
	// suffix separately gives us an exact boundary: only values Codex added
	// after the seal are eligible for promotion.
	var whole map[string]any
	if err := toml.Unmarshal(data, &whole); err != nil {
		return nil, fmt.Errorf("parse managed Codex profile: %w", err)
	}
	suffix := bytes.TrimSpace(data[lineEnd+1:])
	if len(suffix) == 0 {
		return nil, nil
	}
	var added map[string]any
	if err := toml.Unmarshal(suffix, &added); err != nil {
		return nil, fmt.Errorf("parse managed Codex profile additions: %w", err)
	}
	apps, ok := stringMap(added["apps"])
	if !ok {
		return nil, nil
	}

	var approvals []CodexToolApproval
	for appID, rawApp := range apps {
		if !codexAppIDRe.MatchString(appID) {
			continue
		}
		app, ok := stringMap(rawApp)
		if !ok {
			continue
		}
		tools, ok := stringMap(app["tools"])
		if !ok {
			continue
		}
		for toolName, rawTool := range tools {
			tool, ok := stringMap(rawTool)
			if !ok || len(tool) != 1 || tool["approval_mode"] != "approve" || !validCodexToolName(toolName) {
				continue
			}
			approvals = append(approvals, CodexToolApproval{AppID: appID, Tool: toolName})
		}
	}
	sort.Slice(approvals, func(i, j int) bool {
		if approvals[i].AppID != approvals[j].AppID {
			return approvals[i].AppID < approvals[j].AppID
		}
		return approvals[i].Tool < approvals[j].Tool
	})
	return approvals, nil
}

func validCodexToolName(name string) bool {
	if name == "" || len(name) > 256 || !utf8.ValidString(name) {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func stringMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// PromoteCodexLaunchProfileApprovals copies verified app-tool "Always allow"
// additions into the persistent Codex config. It never overwrites an existing
// per-tool decision and never copies unrelated launch-profile changes.
func PromoteCodexLaunchProfileApprovals(profilePath string) (CodexApprovalPromotion, error) {
	var report CodexApprovalPromotion
	if !IsCodexAgentLaunchProfilePath(profilePath) {
		return report, fmt.Errorf("refusing non-managed Codex profile path %q", profilePath)
	}
	fi, err := os.Lstat(profilePath)
	if err != nil {
		return report, fmt.Errorf("inspect managed Codex profile: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return report, fmt.Errorf("managed Codex profile is not a regular file")
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return report, fmt.Errorf("read managed Codex profile: %w", err)
	}
	approvals, err := ExtractCodexLaunchProfileApprovals(data)
	if err != nil {
		return report, &codexLaunchProfileValidationError{err: err}
	}
	report.Found = len(approvals)
	if len(approvals) == 0 {
		return report, nil
	}

	dir, err := codexConfigDir()
	if err != nil {
		return report, err
	}
	configPath := filepath.Join(dir, "config.toml")
	return mergeCodexToolApprovals(configPath, approvals)
}

func mergeCodexToolApprovals(configPath string, approvals []CodexToolApproval) (CodexApprovalPromotion, error) {
	report := CodexApprovalPromotion{Found: len(approvals)}
	err := EditCodexConfigFile(configPath, 0o600, func(data []byte) (bool, []byte, error) {
		report = CodexApprovalPromotion{Found: len(approvals)}
		return planCodexToolApprovals(data, approvals, &report)
	})
	if err != nil {
		return report, fmt.Errorf("persist Codex app-tool approval: %w", err)
	}
	return report, nil
}

func planCodexToolApprovals(data []byte, approvals []CodexToolApproval, report *CodexApprovalPromotion) (bool, []byte, error) {
	var config map[string]any
	if len(bytes.TrimSpace(data)) > 0 {
		if err := toml.Unmarshal(data, &config); err != nil {
			return false, nil, fmt.Errorf("parse Codex config: %w", err)
		}
	} else {
		config = make(map[string]any)
	}

	toAdd := make([]CodexToolApproval, 0, len(approvals))
	for _, approval := range approvals {
		decision, exists, shapeErr := existingCodexToolDecision(config, approval)
		if shapeErr != nil {
			return false, nil, shapeErr
		}
		if exists {
			if decision == "approve" {
				report.Existing++
			} else {
				report.Conflicts = append(report.Conflicts,
					fmt.Sprintf("%s/%s already has approval_mode=%q", approval.AppID, approval.Tool, decision))
			}
			continue
		}
		toAdd = append(toAdd, approval)
	}
	if len(toAdd) == 0 {
		return false, data, nil
	}

	out := append([]byte(nil), data...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	if len(bytes.TrimSpace(out)) > 0 && !bytes.HasSuffix(out, []byte("\n\n")) {
		out = append(out, '\n')
	}
	for i, approval := range toAdd {
		if i > 0 {
			out = append(out, '\n')
		}
		header := "[apps." + approval.AppID + ".tools." + tomlQuote(approval.Tool) + "]\n"
		out = append(out, header...)
		out = append(out, "approval_mode = \"approve\"\n"...)
	}
	// Decoding the original config does not distinguish an inline table from
	// a normal table. Validate the final document so constructs such as
	// `apps = {}` cannot be turned into invalid TOML by an appended header.
	var check map[string]any
	if err := toml.Unmarshal(out, &check); err != nil {
		return false, nil, fmt.Errorf("approval would conflict with existing Codex config shape: %w", err)
	}
	report.Added = len(toAdd)
	return true, out, nil
}

func existingCodexToolDecision(config map[string]any, approval CodexToolApproval) (string, bool, error) {
	rawApps, exists := config["apps"]
	if !exists {
		return "", false, nil
	}
	apps, ok := stringMap(rawApps)
	if !ok {
		return "", false, fmt.Errorf("codex config apps key has a conflicting non-table shape")
	}
	rawApp, exists := apps[approval.AppID]
	if !exists {
		return "", false, nil
	}
	app, ok := stringMap(rawApp)
	if !ok {
		return "", false, fmt.Errorf("codex config app %s has a conflicting non-table shape", approval.AppID)
	}
	rawTools, exists := app["tools"]
	if !exists {
		return "", false, nil
	}
	tools, ok := stringMap(rawTools)
	if !ok {
		return "", false, fmt.Errorf("codex config app %s tools key has a conflicting non-table shape", approval.AppID)
	}
	rawTool, exists := tools[approval.Tool]
	if !exists {
		return "", false, nil
	}
	tool, ok := stringMap(rawTool)
	if !ok {
		return "", false, fmt.Errorf("codex config tool %s/%s has a conflicting non-table shape", approval.AppID, approval.Tool)
	}
	rawDecision, exists := tool["approval_mode"]
	if !exists {
		return "", false, fmt.Errorf("codex config tool %s/%s already exists without approval_mode; refusing a duplicate table", approval.AppID, approval.Tool)
	}
	decision, ok := rawDecision.(string)
	if !ok {
		return "", false, fmt.Errorf("codex config tool %s/%s has a non-string approval_mode", approval.AppID, approval.Tool)
	}
	return decision, true, nil
}
