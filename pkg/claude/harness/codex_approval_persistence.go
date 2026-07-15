package harness

import (
	"bytes"
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

const CodexNoticeHideRateLimitModelNudge = "hide_rate_limit_model_nudge"

var promotedCodexNoticeKeys = map[string]struct{}{
	CodexNoticeHideRateLimitModelNudge: {},
}

// CodexToolApproval is one app-tool "Always allow" choice from a
// launch-specific config profile.
type CodexToolApproval struct {
	AppID string
	Tool  string
}

// CodexNoticePreference is one allowlisted, monotonic Codex notice dismissal
// written into a launch-specific profile. Only true values are promoted: a
// false value means "show this notice" and is not a durable user choice made by
// dismissing a prompt.
type CodexNoticePreference struct {
	Key   string
	Value bool
}

// CodexProfilePromotion reports what a launch-profile reconciliation found.
// Conflicts are deliberately not overwritten: an existing global decision is
// user-owned and wins over the temporary profile.
type CodexProfilePromotion struct {
	Found           int
	Added           int
	Existing        int
	Conflicts       []string
	NoticesFound    int
	NoticesAdded    int
	NoticesExisting int
	NoticeConflicts []string
}

// CodexApprovalPromotion is kept for source compatibility with callers of the
// original app-approval-only reconciliation API.
// Deprecated: use CodexProfilePromotion.
type CodexApprovalPromotion = CodexProfilePromotion

type codexLaunchProfileValidationError struct{ err error }

func (e *codexLaunchProfileValidationError) Error() string { return e.err.Error() }
func (e *codexLaunchProfileValidationError) Unwrap() error { return e.err }

// IsCodexLaunchProfileValidationError reports whether promotion failed because
// the managed profile itself was malformed.
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

// ExtractCodexLaunchProfileApprovals parses a managed profile and returns its
// explicit app-tool "approve" decisions. Unrelated Codex-owned settings (for
// example TUI onboarding state) and non-approve decisions are ignored. The
// profile's permission settings are intentionally not an approval provenance
// check: Codex may rewrite unrelated TOML, while the exact managed path lives
// outside the sandboxed agent's writable roots. A process with ordinary user
// access to this directory could already edit the persistent config directly.
func ExtractCodexLaunchProfileApprovals(data []byte) ([]CodexToolApproval, error) {
	var profile map[string]any
	if err := toml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("parse managed Codex profile: %w", err)
	}
	rawApps, exists := profile["apps"]
	if !exists {
		return nil, nil
	}
	apps, ok := stringMap(rawApps)
	if !ok {
		return nil, fmt.Errorf("managed Codex profile apps key has a non-table shape")
	}

	var approvals []CodexToolApproval
	for appID, rawApp := range apps {
		if !codexAppIDRe.MatchString(appID) {
			continue
		}
		app, ok := stringMap(rawApp)
		if !ok {
			return nil, fmt.Errorf("managed Codex profile app %s has a non-table shape", appID)
		}
		rawTools, exists := app["tools"]
		if !exists {
			continue
		}
		tools, ok := stringMap(rawTools)
		if !ok {
			return nil, fmt.Errorf("managed Codex profile app %s tools key has a non-table shape", appID)
		}
		for toolName, rawTool := range tools {
			if !validCodexToolName(toolName) {
				continue
			}
			tool, ok := stringMap(rawTool)
			if !ok {
				return nil, fmt.Errorf("managed Codex profile tool %s/%s has a non-table shape", appID, toolName)
			}
			rawDecision, exists := tool["approval_mode"]
			if !exists {
				continue
			}
			decision, ok := rawDecision.(string)
			if !ok {
				return nil, fmt.Errorf("managed Codex profile tool %s/%s has a non-string approval_mode", appID, toolName)
			}
			if decision != "approve" {
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

// ExtractCodexLaunchProfileNotices returns allowlisted notice dismissals that
// Codex wrote into a launch-specific profile. Unknown notice keys and false
// values remain profile-local; a malformed notice table is treated like any
// other malformed managed profile.
func ExtractCodexLaunchProfileNotices(data []byte) ([]CodexNoticePreference, error) {
	var profile map[string]any
	if err := toml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("parse managed Codex profile: %w", err)
	}
	rawNotice, exists := profile["notice"]
	if !exists {
		return nil, nil
	}
	notice, ok := stringMap(rawNotice)
	if !ok {
		return nil, fmt.Errorf("managed Codex profile notice key has a non-table shape")
	}

	preferences := make([]CodexNoticePreference, 0, len(promotedCodexNoticeKeys))
	for key := range promotedCodexNoticeKeys {
		rawValue, exists := notice[key]
		if !exists {
			continue
		}
		value, ok := rawValue.(bool)
		if !ok {
			return nil, fmt.Errorf("managed Codex profile notice %s has a non-boolean value", key)
		}
		if value {
			preferences = append(preferences, CodexNoticePreference{Key: key, Value: true})
		}
	}
	sort.Slice(preferences, func(i, j int) bool { return preferences[i].Key < preferences[j].Key })
	return preferences, nil
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

// PromoteCodexLaunchProfileChanges copies explicit app-tool "Always allow"
// decisions and allowlisted Codex notice dismissals into the persistent Codex
// config. It never overwrites an existing global decision and never copies
// unrelated launch-profile settings.
func PromoteCodexLaunchProfileChanges(profilePath string) (CodexProfilePromotion, error) {
	var report CodexProfilePromotion
	data, err := readCodexLaunchProfile(profilePath)
	if err != nil {
		return report, err
	}
	approvals, err := ExtractCodexLaunchProfileApprovals(data)
	if err != nil {
		return report, &codexLaunchProfileValidationError{err: err}
	}
	notices, err := ExtractCodexLaunchProfileNotices(data)
	if err != nil {
		return report, &codexLaunchProfileValidationError{err: err}
	}
	report.Found = len(approvals)
	report.NoticesFound = len(notices)
	if len(approvals) == 0 && len(notices) == 0 {
		return report, nil
	}

	dir, err := codexConfigDir()
	if err != nil {
		return report, err
	}
	configPath := filepath.Join(dir, "config.toml")
	return mergeCodexProfileChanges(configPath, approvals, notices)
}

func readCodexLaunchProfile(profilePath string) ([]byte, error) {
	if !IsCodexAgentLaunchProfilePath(profilePath) {
		return nil, fmt.Errorf("refusing non-managed Codex profile path %q", profilePath)
	}
	fi, err := os.Lstat(profilePath)
	if err != nil {
		return nil, fmt.Errorf("inspect managed Codex profile: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("managed Codex profile is not a regular file")
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, fmt.Errorf("read managed Codex profile: %w", err)
	}
	return data, nil
}

// PromoteCodexLaunchProfileApprovals preserves the original exported entry
// point and its approval-only behavior.
// Deprecated: use PromoteCodexLaunchProfileChanges.
func PromoteCodexLaunchProfileApprovals(profilePath string) (CodexApprovalPromotion, error) {
	var report CodexApprovalPromotion
	data, err := readCodexLaunchProfile(profilePath)
	if err != nil {
		return report, err
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
	return mergeCodexToolApprovals(filepath.Join(dir, "config.toml"), approvals)
}

func mergeCodexProfileChanges(configPath string, approvals []CodexToolApproval, notices []CodexNoticePreference) (CodexProfilePromotion, error) {
	report := CodexProfilePromotion{Found: len(approvals), NoticesFound: len(notices)}
	err := EditCodexConfigFile(configPath, 0o600, func(data []byte) (bool, []byte, error) {
		report = CodexProfilePromotion{Found: len(approvals), NoticesFound: len(notices)}
		return planCodexProfileChanges(data, approvals, notices, &report)
	})
	if err != nil {
		return report, fmt.Errorf("persist Codex launch-profile changes: %w", err)
	}
	return report, nil
}

func mergeCodexToolApprovals(configPath string, approvals []CodexToolApproval) (CodexProfilePromotion, error) {
	return mergeCodexProfileChanges(configPath, approvals, nil)
}

func planCodexProfileChanges(data []byte, approvals []CodexToolApproval, notices []CodexNoticePreference, report *CodexProfilePromotion) (bool, []byte, error) {
	approvalsChanged, out, err := planCodexToolApprovals(data, approvals, report)
	if err != nil {
		return false, nil, err
	}
	noticesChanged, out, err := planCodexNoticePreferences(out, notices, report)
	if err != nil {
		return false, nil, err
	}
	return approvalsChanged || noticesChanged, out, nil
}

func planCodexToolApprovals(data []byte, approvals []CodexToolApproval, report *CodexProfilePromotion) (bool, []byte, error) {
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

func planCodexNoticePreferences(data []byte, notices []CodexNoticePreference, report *CodexProfilePromotion) (bool, []byte, error) {
	if len(notices) == 0 {
		return false, data, nil
	}

	var config map[string]any
	if len(bytes.TrimSpace(data)) > 0 {
		if err := toml.Unmarshal(data, &config); err != nil {
			return false, nil, fmt.Errorf("parse Codex config: %w", err)
		}
	} else {
		config = make(map[string]any)
	}

	var notice map[string]any
	rawNotice, noticeExists := config["notice"]
	if noticeExists {
		var ok bool
		notice, ok = stringMap(rawNotice)
		if !ok {
			return false, nil, fmt.Errorf("codex config notice key has a conflicting non-table shape")
		}
	} else {
		notice = make(map[string]any)
	}

	toAdd := make([]CodexNoticePreference, 0, len(notices))
	for _, preference := range notices {
		if _, allowed := promotedCodexNoticeKeys[preference.Key]; !allowed || !preference.Value {
			continue
		}
		rawValue, exists := notice[preference.Key]
		if !exists {
			toAdd = append(toAdd, preference)
			continue
		}
		value, ok := rawValue.(bool)
		if !ok {
			return false, nil, fmt.Errorf("codex config notice %s has a non-boolean value", preference.Key)
		}
		if value {
			report.NoticesExisting++
		} else {
			report.NoticeConflicts = append(report.NoticeConflicts,
				fmt.Sprintf("notice.%s is already false", preference.Key))
		}
	}
	if len(toAdd) == 0 {
		return false, data, nil
	}
	sort.Slice(toAdd, func(i, j int) bool { return toAdd[i].Key < toAdd[j].Key })

	lines, sep := splitConfigLines(data)
	headerIdx := -1
	for i, line := range lines {
		if name, ok := tomlTableHeader(line); ok && name == "notice" {
			headerIdx = i
			break
		}
	}

	preferenceLines := make([]string, 0, len(toAdd))
	for _, preference := range toAdd {
		preferenceLines = append(preferenceLines, preference.Key+" = true")
	}
	var outLines []string
	switch {
	case headerIdx >= 0:
		outLines = append([]string{}, lines[:headerIdx+1]...)
		outLines = append(outLines, preferenceLines...)
		outLines = append(outLines, lines[headerIdx+1:]...)
	case noticeExists:
		return false, nil, fmt.Errorf("codex config notice table uses a form tclaude cannot safely extend")
	default:
		outLines = append([]string{}, lines...)
		if len(outLines) > 0 && strings.TrimSpace(outLines[len(outLines)-1]) != "" {
			outLines = append(outLines, "")
		}
		outLines = append(outLines, "[notice]")
		outLines = append(outLines, preferenceLines...)
	}
	out := joinConfigLines(outLines, sep)
	var check map[string]any
	if err := toml.Unmarshal(out, &check); err != nil {
		return false, nil, fmt.Errorf("notice preference would conflict with existing Codex config shape: %w", err)
	}
	report.NoticesAdded = len(toAdd)
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
